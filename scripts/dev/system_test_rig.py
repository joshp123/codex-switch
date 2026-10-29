"""The isolated system the system test runs: a private copy of the proxy under
launchd, the gateway to real OpenAI, the official clients, and readers for the
evidence each of them leaves."""
import base64
from datetime import datetime
import hashlib
import http.client
import json
import os
from pathlib import Path
import plistlib
import queue
import re
import select
import shutil
import signal
import socket
import socketserver
import sqlite3
import subprocess
import threading
import time
import urllib.request

REPO = Path(__file__).resolve().parents[2]
RUN = REPO / '.project-private/system-test'
LIVE_DATA = Path.home() / 'Library/Application Support/codex-switch'
# The Codex client the proxy itself reads: the one inside the desktop app, else `codex` on PATH.
DESKTOP_CODEX = Path('/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex')
CODEX = str(DESKTOP_CODEX) if DESKTOP_CODEX.exists() else shutil.which('codex')
PI = shutil.which('pi')  # None when Pi is not installed; the test then runs without it
CLAUDE_DESKTOP = Path.home() / 'Library/Application Support/Claude/claude-code'
CLAUDE_VERSIONS = sorted(CLAUDE_DESKTOP.glob('*/**/claude.app/Contents/MacOS/claude'),
                         key=lambda path: tuple(map(int, path.relative_to(CLAUDE_DESKTOP).parts[0].split('.'))))
CLAUDE = str(CLAUDE_VERSIONS[-1]) if CLAUDE_VERSIONS else shutil.which('claude')
CLAUDE_ACCOUNT = None
CLAUDE_SKIP_REASON = 'no Claude account is signed in' if CLAUDE else 'Claude Code is not installed'
MODEL = 'gpt-6-luna'
LABEL = 'dev.codex-switch.system-test-' + hashlib.sha256(str(REPO).encode()).hexdigest()[:10] + '-terra'
# Pi has no ChatGPT login of its own here: it needs a JWT-shaped key with a
# chatgpt_account_id claim. The proxy replaces the credentials it carries.
PI_PLACEHOLDER_KEY = 'eyJhbGciOiJub25lIn0.eyJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9hY2NvdW50X2lkIjoiY29kZXgtc3dpdGNoIn19.codex-switch'

started_processes = []
started_tunnels = []


class Failure(Exception):
    """An outcome did not happen; the message says what happened instead."""


# Setup


def prepare():
    """Creates a fresh run directory with copies of the account logins."""
    global CLAUDE_ACCOUNT, CLAUDE_SKIP_REASON
    os.umask(0o077)  # logins, logs and the proxy's data stay private to this user
    if not CODEX:
        raise Failure('Codex is not installed: neither the desktop app nor `codex` on PATH.')
    if RUN.exists():
        subprocess.run(['trash', str(RUN)], check=True)
    for part in ('home', 'work', 'data', 'logs', 'pi', 'claude'):
        (RUN / part).mkdir(parents=True)
    accounts = []
    for source in sorted(LIVE_DATA.glob('account-*/auth.json')):
        auth = json.loads(source.read_text())
        if auth.get('provider') == 'claude':
            if not CLAUDE or CLAUDE_ACCOUNT or not all(auth.get(field) for field in
                    ('access_token', 'refresh_token', 'expires_at', 'account_id', 'organization_id', 'email')):
                continue
            if datetime.fromisoformat(auth['expires_at'].replace('Z', '+00:00')).timestamp() <= time.time() + 3600:
                CLAUDE_SKIP_REASON = 'the signed-in Claude token expires within an hour'
                continue
            auth['refresh_token'] = 'system-test-no-refresh'
        else:
            claims = json.loads(base64.urlsafe_b64decode(auth['tokens']['access_token'].split('.')[1] + '==='))
            if claims['exp'] < time.time() + 3600:
                raise Failure(f'{source.parent.name}\'s access token expires in {int((claims["exp"] - time.time()) / 60)} minutes; '
                              'the live proxy renews it when it expires. Run the system test after that.')
            auth['tokens']['refresh_token'] = 'system-test-no-refresh'
        # Only access tokens are copied: a refresh here would rotate the live
        # proxy's refresh token and sign the live proxy out.
        target = RUN / 'data' / source.parent.name / 'auth.json'
        target.parent.mkdir()
        target.write_text(json.dumps(auth))
        if auth.get('provider') == 'claude':
            CLAUDE_ACCOUNT = source.parent.name
        else:
            accounts.append(source.parent.name)
    if len(accounts) < 2:
        raise Failure('The system test needs at least two signed-in accounts.')
    (RUN / 'home/auth.json').write_text((RUN / 'data' / accounts[0] / 'auth.json').read_text())
    (RUN / 'work/input.txt').write_text('3\n5\n8\n')
    subprocess.run(['git', 'init', '-q', str(RUN / 'work')], check=True)
    return accounts


def build():
    """Build the same source with distinct release numbers and the traffic gateway."""
    base = int(subprocess.check_output(['git', 'rev-list', '--count', 'HEAD'], cwd=REPO, text=True))
    builds = [(str(RUN / name), base + number) for number, name in enumerate(('codex-switch', 'candidate', 'candidate-2', 'candidate-3', 'candidate-4', 'candidate-5'))]
    commands = [(['scripts/dev/build-proxy', path], dict(os.environ, CODEX_SWITCH_BUILD=str(number))) for path, number in builds]
    commands.append((['go', 'build', '-o', str(RUN / 'gateway'), './scripts/dev/system-test-gateway'], os.environ))
    for command, environment in commands:
        built = subprocess.run(['devenv', 'shell', '--', *command], cwd=REPO, env=environment, capture_output=True, text=True)
        if built.returncode:
            raise Failure(f'Build failed: {" ".join(command)}\n{built.stderr[-1500:]}')


def start_gateway():
    log = (RUN / 'logs/gateway.log').open('w')
    started_processes.append(subprocess.Popen([str(RUN / 'gateway'), '--data-dir', str(RUN / 'data'), '--control', str(RUN / 'control')],
                                              stdout=log, stderr=log, start_new_session=True))
    origin = wait_for(lambda: re.search(r'http://127\.0\.0\.1:\d+', read('logs/gateway.log')), 'the gateway to listen', timeout=30)
    (RUN / 'gateway-origin').write_text(origin[0])


def start_proxy():
    with socket.socket() as reservation:
        reservation.bind(('127.0.0.1', 0))
        (RUN / 'listen').write_text(f'127.0.0.1:{reservation.getsockname()[1]}')
    plist = RUN / 'home/Library/LaunchAgents' / f'{LABEL}.plist'
    plist.parent.mkdir(parents=True)
    plist.write_bytes(plistlib.dumps({
        'Label': LABEL,
        'ProgramArguments': [str(RUN / 'codex-switch'), 'serve', '--listen', listen_address()],
        'EnvironmentVariables': proxy_environment(),
        'KeepAlive': True,
        'RunAtLoad': True,
        'ExitTimeOut': 0,  # launchd must never kill admitted work after a deadline
        'StandardOutPath': str(RUN / 'logs/launchd.log'),
        'StandardErrorPath': str(RUN / 'logs/launchd.log'),
    }))
    for label in (LABEL, LABEL + '.a', LABEL + '.b'):
        subprocess.run(['launchctl', 'bootout', f'gui/{os.getuid()}/{label}'], capture_output=True)
    subprocess.run(['launchctl', 'bootstrap', f'gui/{os.getuid()}', str(plist)], check=True)
    wait_for(deploy_status, 'the proxy to serve (see data/proxy.log)', timeout=120)
    base = f'http://{listen_address()}/client/{read("data/client-capability").strip()}/backend-api/codex'
    (RUN / 'home/config.toml').write_text(
        f'openai_base_url = "{base}"\nmodel = "{MODEL}"\nmodel_reasoning_effort = "low"\n'
        'cli_auth_credentials_store = "file"\n[analytics]\nenabled = false\n')
    (RUN / 'pi/models.json').write_text(json.dumps({'providers': {'openai-codex': {'baseUrl': base, 'apiKey': PI_PLACEHOLDER_KEY}}}))
    return base


def proxy_environment():
    # PATH lets the proxy find a Codex CLI installed without the desktop app.
    return {'CODEX_SWITCH_DATA_DIR': str(RUN / 'data'), 'CODEX_SWITCH_UPSTREAM_ORIGIN': read('gateway-origin'),
            'CODEX_HOME': str(RUN / 'home'), 'PI_CODING_AGENT_DIR': str(RUN / 'pi'), 'PATH': os.environ['PATH']}


def client_environment():
    environment = {name: value for name, value in os.environ.items() if not name.startswith(('CODEX_', 'OPENAI_', 'PI_'))}
    environment.update(CODEX_HOME=str(RUN / 'home'), PI_CODING_AGENT_DIR=str(RUN / 'pi'))
    return environment


def clean_up():
    """Stops every client, proxy generation and the gateway; returns anything still running."""
    for name in (signal.SIGINT, signal.SIGTERM):
        signal.signal(name, signal.SIG_IGN)  # a second interrupt must not stop the cleanup
    for tunnel in started_tunnels:
        tunnel.close()
    gateway = str(RUN / 'gateway')
    if (RUN / 'control').exists():
        control()  # release held responses so draining processes can finish
    for process in started_processes:
        if process.args[0] != gateway:
            stop_process_group(process)
    for label in (LABEL, LABEL + '.a', LABEL + '.b'):
        subprocess.run(['launchctl', 'bootout', f'gui/{os.getuid()}/{label}'], capture_output=True)
    for pid in run_processes(exclude=gateway):
        try:
            os.kill(pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
    wait_for(lambda: not run_processes(exclude=gateway), 'proxy processes to stop', timeout=60, fail=False)
    for process in started_processes:
        stop_process_group(process)
    leftovers = [f'process {pid}' for pid in run_processes()]
    for label in (LABEL, LABEL + '.a', LABEL + '.b'):
        if subprocess.run(['launchctl', 'print', f'gui/{os.getuid()}/{label}'], capture_output=True).returncode == 0:
            leftovers.append('launchd job ' + label)
    return leftovers


def stop_process_group(process):
    for stop_signal in (signal.SIGTERM, signal.SIGKILL):
        if process.poll() is not None:
            return
        try:
            os.killpg(process.pid, stop_signal)
            process.wait(timeout=10)
        except (ProcessLookupError, subprocess.TimeoutExpired):
            pass


def run_processes(exclude=None):
    """Processes running an executable from this run's directory."""
    listing = subprocess.run(['ps', '-axo', 'pid=,stat=,command='], capture_output=True, text=True, check=True).stdout
    owned = []
    for line in listing.splitlines():
        pid, state, command = line.split(None, 2)
        if command.startswith(str(RUN) + '/') and not state.startswith('Z') and not (exclude and command.startswith(exclude)):
            owned.append(int(pid))
    return owned


# Clients


def start_client(command, name, **options):
    options.setdefault('stdin', subprocess.DEVNULL)
    options.setdefault('stdout', (RUN / f'logs/{name}.out').open('w'))
    options.setdefault('env', client_environment())
    process = subprocess.Popen(command, cwd=RUN / 'work', stderr=(RUN / f'logs/{name}.err').open('w'),
                               start_new_session=True, **options)
    started_processes.append(process)
    return process


class CodexTask:
    """One `codex exec` run of the official Codex client, in the background."""

    def __init__(self, name, prompt, thread=None):
        self.name = name
        command = [CODEX, 'exec'] + (['resume', thread] if thread else ['--sandbox', 'workspace-write'])
        self.process = start_client(command + ['--json', '--skip-git-repo-check', prompt], name)

    def finish(self, timeout=300):
        try:
            self.process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            raise Failure(f'Codex task {self.name} did not finish within {timeout} seconds.')
        self.events = [json.loads(line) for line in read(f'logs/{self.name}.out').splitlines() if line.startswith('{')]
        self.thread = next((event['thread_id'] for event in self.events if event.get('type') == 'thread.started'), None)
        self.errors = [event for event in self.events if event.get('type') in ('error', 'turn.failed')]
        self.reply = next((event['item']['text'] for event in reversed(self.events)
                           if event.get('type') == 'item.completed' and event['item'].get('type') == 'agent_message'), '')
        if self.process.returncode or not self.thread:
            raise Failure(f'Codex task {self.name} exited with status {self.process.returncode}: '
                          f'{self.errors or read(f"logs/{self.name}.err")[-400:]}')
        return self


def start_pi(name, prompt, session):
    """Starts one prompt in a Pi session, as `pi -p` does for a person."""
    return start_client([PI, '--provider', 'openai-codex', '--model', MODEL + ':low', '--offline', '--no-extensions',
                         '--no-skills', '--no-prompt-templates', '--no-context-files', '--no-mcp',
                         '--session-dir', str(RUN / 'pi/sessions'), '--session-id', session, '-p', prompt], name)


def finish_pi(name, process, timeout=300):
    if process.wait(timeout=timeout):
        raise Failure(f'Pi run {name} exited with status {process.returncode}: {read(f"logs/{name}.err")[-400:]}')
    return read(f'logs/{name}.out').strip()


def run_pi(name, prompt, session):
    return finish_pi(name, start_pi(name, prompt, session))


def run_claude(session):
    """Claude Code through the two printed proxy settings, to real Anthropic over TLS."""
    environment = {name: value for name, value in client_environment().items()
                   if not name.upper().startswith(('CLAUDE_', 'ANTHROPIC_'))
                   and not name.upper().endswith('_PROXY')
                   and name not in ('NODE_EXTRA_CA_CERTS', 'NODE_TLS_REJECT_UNAUTHORIZED')}
    environment.update(HOME=str(RUN / 'claude'), CLAUDE_CONFIG_DIR=str(RUN / 'claude'),
                       CODEX_SWITCH_DATA_DIR=str(RUN / 'data'))
    settings = subprocess.run([str(RUN / 'codex-switch'), 'print-claude-config', '--listen', listen_address()],
                              env=environment, capture_output=True, text=True, check=True)
    environment.update(json.loads(settings.stdout)['env'])
    auth = json.loads((RUN / 'data' / CLAUDE_ACCOUNT / 'auth.json').read_text())
    environment['CLAUDE_CODE_OAUTH_TOKEN'] = auth['access_token']
    process = start_client([CLAUDE, '-p', 'Reply with the word ready only.', '--model', 'haiku', '--effort', 'low',
                            '--output-format', 'json', '--session-id', session, '--tools', '',
                            '--setting-sources', '', '--strict-mcp-config'], 'claude', env=environment)
    if process.wait(timeout=180):
        raise Failure(f'Claude Code exited with status {process.returncode}: {read("logs/claude.err")[-400:]} '
                      f'{read("logs/claude.out")[-400:]}')
    return json.loads(read('logs/claude.out'))


def pi_task_id(session):
    """The proxy's task ID for a Pi session, as internal/taskidentity derives it."""
    identity = bytearray(hashlib.sha256(('codex-switch/pi-session/' + session).encode()).digest())
    identity[6] = identity[6] & 0x0f | 0x80
    identity[8] = identity[8] & 0x3f | 0x80
    hexadecimal = identity[:16].hex()
    return '-'.join((hexadecimal[:8], hexadecimal[8:12], hexadecimal[12:16], hexadecimal[16:20], hexadecimal[20:]))


def post_without_task_id(base, prompt):
    """Sends one streamed Responses request with no task ID, as a client without tasks would."""
    body = {'model': MODEL, 'instructions': 'Answer briefly.', 'stream': True, 'store': False, 'reasoning': {'effort': 'low'},
            'input': [{'type': 'message', 'role': 'user', 'content': [{'type': 'input_text', 'text': prompt}]}]}
    request = urllib.request.Request(base + '/responses', data=json.dumps(body).encode(),
                                     headers={'Content-Type': 'application/json', 'Accept': 'text/event-stream'})
    with urllib.request.urlopen(request, timeout=180) as response:
        events = [json.loads(line[6:]) for line in response.read().decode().splitlines() if line.startswith('data: {')]
    completed = [event['response'] for event in events if event.get('type') == 'response.completed']
    return response.status, completed


class Goal:
    """One /goal in the official Codex app-server, started once and then left to run by itself."""

    def __init__(self, name, objective):
        self.name, self.notifications, self.responses, self.next_id = name, [], queue.Queue(), 0
        self.effect_file, self.result_file = RUN / f'work/{name}-effects.txt', RUN / f'work/{name}-result.txt'
        self.process = start_client([CODEX, 'app-server', '--stdio'], name, stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
        threading.Thread(target=self.read_messages, daemon=True).start()
        self.request('initialize', {'clientInfo': {'name': 'codex_switch_system_test', 'version': '1'}, 'capabilities': {'experimentalApi': True}})
        self.send({'method': 'initialized', 'params': {}})
        self.thread = self.request('thread/start', {'cwd': str(RUN / 'work'), 'model': MODEL, 'sandbox': 'workspace-write',
                                                    'approvalPolicy': 'never'})['thread']['id']
        objective = objective.format(effect=self.effect_file.name, result=self.result_file.name)
        self.request('thread/goal/set', {'threadId': self.thread, 'objective': objective, 'tokenBudget': 200000})
        # The first turn is the person pressing send; every later turn belongs to the goal.
        self.request('turn/start', {'threadId': self.thread, 'input': [{'type': 'text', 'text': 'Work on the active goal.'}]})

    def read_messages(self):
        with (RUN / f'logs/{self.name}.jsonl').open('w') as log:
            for line in self.process.stdout:
                log.write(line)
                log.flush()
                message = json.loads(line)
                (self.responses.put(message) if 'id' in message and 'method' not in message else self.notifications.append(message))

    def send(self, message):
        self.process.stdin.write(json.dumps(message) + '\n')
        self.process.stdin.flush()

    def request(self, method, params):
        self.next_id += 1
        self.send({'id': self.next_id, 'method': method, 'params': params})
        while True:
            message = self.responses.get(timeout=60)
            if message['id'] == self.next_id:
                if 'error' in message:
                    raise Failure(f'{self.name}: app-server refused {method}: {message["error"]}')
                return message['result']

    def ended(self):
        """The goal's final status, once it has one and its last turn has finished."""
        status = self.request('thread/goal/get', {'threadId': self.thread}).get('goal', {}).get('status')
        turns = [message['method'] for message in self.notifications if message.get('method') in ('turn/started', 'turn/completed')]
        if status in ('complete', 'blocked', 'usageLimited', 'budgetLimited') and turns.count('turn/started') == turns.count('turn/completed'):
            return status
        return None

    def finish(self, timeout=300):
        """Waits for the goal to end by itself; returns the errors Codex showed."""
        status = wait_for(self.ended, f'goal {self.name} to finish', timeout=timeout, interval=5)
        if status != 'complete':
            raise Failure(f'Goal {self.name} ended as {status}, not complete.')
        effects = self.effect_file.read_text() if self.effect_file.exists() else ''
        if effects.count('START') != 1:
            raise Failure(f'Goal {self.name} ran its one-time tool effect {effects.count("START")} times.')
        if self.result_file.read_text().strip() != '16':
            raise Failure(f'Goal {self.name} wrote {self.result_file.read_text().strip()!r}, not the sum 16.')
        self.process.stdin.close()
        self.process.wait(timeout=30)
        return [message for message in self.notifications if message.get('method') == 'error']


class TunnelPair:
    """Two CONNECT tunnels to an echo server, one transferring and one idle."""
    def __init__(self):
        class Echo(socketserver.BaseRequestHandler):
            def handle(self):
                while data := self.request.recv(4096):
                    self.request.sendall(data)
        self.server = socketserver.ThreadingTCPServer(('127.0.0.1', 0), Echo)
        self.server.daemon_threads = True
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.stopped, self.error = threading.Event(), None
        self.sockets = []
        started_tunnels.append(self)
        target = f'127.0.0.1:{self.server.server_address[1]}'
        capability = read('data/client-capability').strip()
        auth = base64.b64encode(('codex-switch:' + capability).encode()).decode()
        host, port = listen_address().split(':')
        self.idle_http = http.client.HTTPConnection(host, int(port), timeout=10)
        self.idle_http.request('GET', f'/client/{capability}/codex-switch/deploy-status')
        response = self.idle_http.getresponse()
        response.read()
        if response.status != 200 or self.idle_http.sock is None:
            raise Failure('Could not establish the idle HTTP keep-alive connection.')
        for _ in range(2):
            connection = socket.create_connection((host, int(port)))
            connection.sendall(f'CONNECT {target} HTTP/1.1\r\nHost: {target}\r\nProxy-Authorization: Basic {auth}\r\n\r\n'.encode())
            response = b''
            while not response.endswith(b'\r\n\r\n'):
                part = connection.recv(1)
                if not part:
                    raise Failure('The proxy closed the CONNECT handshake.')
                response += part
            if not response.startswith(b'HTTP/1.1 200'):
                raise Failure(f'The proxy rejected a CONNECT tunnel: {response!r}')
            self.sockets.append(connection)
        threading.Thread(target=self.transfer, daemon=True).start()

    def transfer(self):
        connection = self.sockets[0]
        connection.settimeout(10)
        try:
            while not self.stopped.is_set():
                connection.sendall(b'work')
                if connection.recv(4) != b'work':
                    raise OSError('tunnel closed during transfer')
                self.stopped.wait(5)
        except OSError as error:
            self.error = error

    def check_idle_open(self):
        idle = self.sockets[1]
        idle.settimeout(65)
        try:
            if idle.recv(1) != b'':
                raise Failure('The idle tunnel returned unexpected data.')
            raise Failure('The silent tunnel was closed before the deadline.')
        except (ConnectionResetError, BrokenPipeError):
            raise Failure('The silent tunnel was closed before the deadline.')
        except socket.timeout:
            pass
        if self.error:
            raise Failure(f'The transferring tunnel was cut: {self.error}')

    def idle_closed(self):
        idle = self.sockets[1]
        if not select.select([idle], [], [], 0)[0]:
            return False
        try:
            return idle.recv(1) == b''
        except ConnectionResetError:
            return True

    def idle_http_closed(self):
        connection = self.idle_http.sock
        if connection is None:
            return True
        if not select.select([connection], [], [], 0)[0]:
            return False
        try:
            return connection.recv(1) == b''
        except ConnectionResetError:
            return True

    def close(self):
        if self.stopped.is_set():
            return
        self.stopped.set()
        if hasattr(self, 'idle_http'):
            self.idle_http.close()
        for connection in self.sockets:
            connection.close()
        self.server.shutdown()
        self.server.server_close()


# Evidence


def database_rows(task):
    """Completed responses the proxy recorded for a task, oldest first."""
    with sqlite3.connect(f'file:{RUN / "data/codex-switch.db"}?mode=ro', uri=True) as database:
        return database.execute('SELECT account_name, input_tokens, cached_input_tokens, output_tokens, reasoning_tokens '
                                'FROM completed_responses WHERE codex_task_id = ? ORDER BY completed_at_unix_nano', (task,)).fetchall()


def database_row_count():
    with sqlite3.connect(f'file:{RUN / "data/codex-switch.db"}?mode=ro', uri=True) as database:
        return database.execute('SELECT count(*) FROM completed_responses').fetchone()[0]


def pinned_account(task):
    with sqlite3.connect(f'file:{RUN / "data/codex-switch.db"}?mode=ro', uri=True) as database:
        row = database.execute('SELECT account_name FROM task_account_pins WHERE codex_task_id = ?', (task,)).fetchone()
    return row[0] if row else None


def gateway_events(event, **only):
    """Gateway log lines for one event, as dictionaries, optionally only those with the given field values."""
    found = []
    for line in read('logs/gateway.log').splitlines():
        fields = {name: value.strip('"') for name, value in re.findall(r'(\w+)=("[^"]*"|\S+)', line)}
        if fields.get('event') == event and all(fields.get(name) == value for name, value in only.items()):
            found.append(fields)
    return found


def forwarded_requests(**only):
    """(account, request fingerprint) for every model request the gateway sent to OpenAI, in order."""
    return [(event['account'], event['request']) for event in gateway_events('forwarded', **only)]


def codex_client_usage(thread):
    """Token usage of every response the Codex client received, from its own session file."""
    usage = []
    for path in (RUN / 'home/sessions').rglob(f'rollout-*-{thread}.jsonl'):
        for line in path.read_text().splitlines():
            payload = json.loads(line).get('payload', {})
            if payload.get('type') == 'token_count' and payload.get('info'):
                last = payload['info']['last_token_usage']
                usage.append((last['input_tokens'], last['cached_input_tokens'], last['output_tokens'], last['reasoning_output_tokens']))
    return usage


def pi_client_usage(session):
    """Token usage of every response Pi received, from its own session file."""
    usage = []
    for path in (RUN / 'pi/sessions').glob(f'*_{session}.jsonl'):
        for line in path.read_text().splitlines():
            message = json.loads(line).get('message', {})
            if message.get('role') == 'assistant' and message.get('usage'):
                tokens = message['usage']
                usage.append((tokens['input'] + tokens['cacheRead'], tokens['cacheRead'], tokens['output'], tokens['reasoning']))
    return usage


def proxy_command(*arguments):
    """Runs a person-facing command against this run's isolated account data."""
    environment = dict(client_environment(), CODEX_SWITCH_DATA_DIR=str(RUN / 'data'))
    result = subprocess.run([str(RUN / 'codex-switch'), *arguments], env=environment, capture_output=True, text=True)
    if result.returncode:
        raise Failure(f'codex-switch {" ".join(arguments)} failed: {result.stderr.strip()}')
    return result.stdout


def account_keys_by_name():
    return dict(line.split('\t')[:2] for line in proxy_command('accounts').splitlines() if '\t' in line)


def weekly_used_percent():
    """Weekly allowance used per account, as `codex-switch status` shows it to a person."""
    environment = dict(client_environment(), CODEX_SWITCH_DATA_DIR=str(RUN / 'data'))
    shown = subprocess.run([str(RUN / 'codex-switch'), 'status'], env=environment,
                           capture_output=True, text=True).stdout
    key_by_name = account_keys_by_name()
    used, account = {}, None
    for line in shown.splitlines():
        if match := re.match(r'((Codex|Claude) \d+) ·', line):
            account = key_by_name[match[1]] if match[2] == 'Codex' else None
        elif account and (match := re.match(r'\s+weekly\s+([\d.]+)% used', line)):
            used[account] = float(match[1])
    return used


def proxy_log():
    return read('data/proxy.log')


def deploy_status():
    try:
        capability = read('data/client-capability').strip()
        with urllib.request.urlopen(f'http://{listen_address()}/client/{capability}/codex-switch/deploy-status', timeout=5) as response:
            return json.load(response)
    except (OSError, ValueError):
        return None


def gateway_connections(pid):
    """The local addresses of a proxy process's open connections to the gateway, as the gateway logs them in `remote`."""
    port = read('gateway-origin').rsplit(':', 1)[1]
    listing = subprocess.run(['lsof', '-nP', '-Fn', '-a', '-p', str(pid), f'-iTCP:{port}', '-sTCP:ESTABLISHED'], capture_output=True, text=True)
    return {line[1:].split('->')[0] for line in listing.stdout.splitlines() if line.startswith('n')}


def process_alive(pid):
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False


# Small helpers


def read(name):
    return (RUN / name).read_text()


def listen_address():
    return read('listen')


def launchd_target():
    return f'gui/{os.getuid()}/{LABEL}'


def control(*lines):
    """Replaces the gateway's simulation file in one step; no lines means real OpenAI behaviour."""
    (RUN / 'control.next').write_text(''.join(line + '\n' for line in lines))
    os.replace(RUN / 'control.next', RUN / 'control')


def wait_for(probe, what, timeout=180, fail=True, interval=0.5):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = probe()
        if value:
            return value
        time.sleep(interval)
    if fail:
        raise Failure(f'Timed out after {timeout} seconds waiting for {what}.')
    return None
