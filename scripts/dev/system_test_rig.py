"""One isolated system the system test runs: a private copy of the proxy under
launchd, the fake providers in place of OpenAI and Anthropic, the official
clients, and readers for the evidence each of them leaves. The system test
runs several at once, each in its own run directory."""
import base64
import hashlib
import http.client
import json
import os
from pathlib import Path
import plistlib
import queue
import re
import secrets
import select
import shlex
import shutil
import signal
import socket
import socketserver
import sqlite3
import subprocess
import threading
import time
import tomllib
import urllib.request

REPO = Path(__file__).resolve().parents[2]
ROOT = REPO / '.project-private/system-test'
# The system test gives each isolated system its own run directory.
RUN = Path(os.environ.get('CODEX_SWITCH_SYSTEM_TEST_RUN', ROOT / 'run'))
BUILDS = ROOT / 'builds'
# The smoke test runs one system against the real services, with copies of the live logins.
REAL_SERVICES = os.environ.get('CODEX_SWITCH_REAL_SERVICES') == '1'
LIVE_DATA = Path.home() / 'Library/Application Support/codex-switch'
# Published releases, kept between runs by their tags.
RELEASES = REPO / '.project-private/system-test-releases'
PUBLISHED_RELEASE_PROXY = BUILDS / 'published-release/CodexSwitchMenu.app/Contents/Helpers/codex-switch'
# The last release that names accounts only by their folders, so an update from it must give its records account keys.
KEYLESS_RELEASE_TAG = 'v0.1.9'
KEYLESS_RELEASE_PROXY = BUILDS / 'keyless-release/CodexSwitchMenu.app/Contents/Helpers/codex-switch'
# The Codex client the proxy itself reads: the one inside the desktop app, else `codex` on PATH.
DESKTOP_CODEX = Path('/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex')
CODEX = str(DESKTOP_CODEX) if DESKTOP_CODEX.exists() else shutil.which('codex')
PI = shutil.which('pi')  # None when Pi is not installed; the test then runs without it
CLAUDE_DESKTOP = Path.home() / 'Library/Application Support/Claude/claude-code'
CLAUDE_VERSIONS = sorted(CLAUDE_DESKTOP.glob('*/**/claude.app/Contents/MacOS/claude'),
                         key=lambda path: tuple(map(int, path.relative_to(CLAUDE_DESKTOP).parts[0].split('.'))))
CLAUDE = str(CLAUDE_VERSIONS[-1]) if CLAUDE_VERSIONS else shutil.which('claude')
CODEX_ACCOUNTS = ['account-1', 'account-2', 'account-3']
CLAUDE_ACCOUNTS = ['account-4', 'account-5', 'account-6']
# Proxy timers, shortened as the installer's are in system-test. The fake
# providers answer at once, so they can be this short.
ACCOUNT_READ_SECONDS, CONNECTION_DRAIN_SECONDS, MENU_SNAPSHOT_SECONDS = 1, 1, 1
MODEL = 'gpt-6-luna'
LABEL = 'dev.codex-switch.system-test-' + hashlib.sha256(str(RUN).encode()).hexdigest()[:10]
# A second proxy, as on Josh's other Mac, signed in to the same accounts.
OTHER_MAC = RUN / 'other-mac'
OTHER_MAC_LABEL = LABEL + '.other-mac'
# Every launchd job a system may start: the proxy, the installer's two slots and the other Mac's proxy.
LABELS = (LABEL, LABEL + '.a', LABEL + '.b', OTHER_MAC_LABEL)
# Pi has no ChatGPT login of its own here: it needs a JWT-shaped key with a
# chatgpt_account_id claim. The proxy replaces the credentials it carries.
PI_PLACEHOLDER_KEY = 'eyJhbGciOiJub25lIn0.eyJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9hY2NvdW50X2lkIjoiY29kZXgtc3dpdGNoIn19.codex-switch'
BUILD_NAMES = ('cli', 'candidate', 'candidate-2', 'candidate-3', 'candidate-4', 'candidate-5', 'candidate-6')

started_processes = []
started_tunnels = []


class Failure(Exception):
    """An outcome did not happen; the message says what happened instead."""


# Setup


def build():
    """Builds the same source with distinct release numbers, and the fake providers, once for every isolated system;
    and fetches the newest published release, so one system's first update is one Macs really make, and the last
    release before account keys."""
    BUILDS.mkdir(parents=True)
    newest = subprocess.Popen(['gh', 'release', 'view', '--repo', 'joshp123/codex-switch', '--json', 'tagName', '--jq', '.tagName'],
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    base = int(subprocess.check_output(['git', 'rev-list', '--count', 'HEAD'], cwd=REPO, text=True))
    # One devenv shell runs every build at once; separate devenv shells race on its state.
    script = ' '.join(f'CODEX_SWITCH_BUILD={base + number} scripts/dev/build-proxy {shlex.quote(str(BUILDS / name))} & '
                      for number, name in enumerate(BUILD_NAMES))
    script += f'go build -o {shlex.quote(str(BUILDS / "fake-providers"))} ./scripts/dev/fake-providers & '
    script += 'failed=0; for job in $(jobs -p); do wait "$job" || failed=1; done; exit $failed'
    built = subprocess.run(['devenv', 'shell', '--', 'bash', '-c', script], cwd=REPO, capture_output=True, text=True)
    if built.returncode:
        raise Failure(f'Build failed:\n{built.stderr[-1500:]}')
    tag, problem = newest.communicate()
    if newest.returncode:
        raise Failure(f'Could not find the newest published release: {problem[-400:]}')
    (BUILDS / 'published-release').symlink_to(fetch_release(tag.strip()))
    (BUILDS / 'keyless-release').symlink_to(fetch_release(KEYLESS_RELEASE_TAG))


def fetch_release(tag):
    release = RELEASES / tag
    if not (release / 'CodexSwitchMenu.app').exists():
        subprocess.run(['gh', 'release', 'download', tag, '--repo', 'joshp123/codex-switch', '--pattern', 'CodexSwitchMenu.zip',
                        '--dir', str(release)], check=True, capture_output=True)
        subprocess.run(['ditto', '-x', '-k', str(release / 'CodexSwitchMenu.zip'), str(release)], check=True)
    return release


def prepare(first_proxy):
    """Creates this system's run directory with fake accounts. first_proxy is 'published-release', 'keyless-release' or
    'cli', the build its launchd job starts with."""
    os.umask(0o077)  # logins, logs and the proxy's data stay private to this user
    if not CODEX:
        raise Failure('Codex is not installed: neither the desktop app nor `codex` on PATH.')
    for part in ('home', 'work', 'data', 'logs', 'logs/requests', 'pi', 'claude'):
        (RUN / part).mkdir(parents=True)
    for name in (*BUILD_NAMES, 'fake-providers'):
        os.link(BUILDS / name, RUN / name)
    first = {'published-release': PUBLISHED_RELEASE_PROXY, 'keyless-release': KEYLESS_RELEASE_PROXY, 'cli': BUILDS / 'cli'}[first_proxy]
    shutil.copy2(first, RUN / 'codex-switch')
    for number, account in enumerate(CODEX_ACCOUNTS, 1):
        (RUN / 'data' / account).mkdir()
        (RUN / 'data' / account / 'auth.json').write_text(json.dumps(fake_codex_login(number)))
    for number, account in enumerate(CLAUDE_ACCOUNTS, 1):
        (RUN / 'data' / account).mkdir()
        (RUN / 'data' / account / 'auth.json').write_text(json.dumps(fake_claude_login(number)))
    (RUN / 'home/auth.json').write_text((RUN / 'data' / CODEX_ACCOUNTS[0] / 'auth.json').read_text())
    (RUN / 'work/input.txt').write_text('3\n5\n8\n')
    subprocess.run(['git', 'init', '-q', str(RUN / 'work')], check=True)
    return list(CODEX_ACCOUNTS)


def prepare_real_services():
    """Creates the smoke test's run directory with copies of the live logins' access tokens. Refresh tokens stay
    behind, so the test cannot rotate a live login; each copied token must outlast the run."""
    os.umask(0o077)
    for part in ('home', 'work', 'data', 'logs', 'pi', 'claude'):
        (RUN / part).mkdir(parents=True)
    os.link(BUILDS / 'cli', RUN / 'cli')
    shutil.copy2(BUILDS / 'cli', RUN / 'codex-switch')
    codex, claude = [], []
    for source in sorted(LIVE_DATA.glob('account-*/auth.json')):
        auth = json.loads(source.read_text())
        if auth.get('provider') == 'claude':
            if time.time() + 600 >= time.mktime(time.strptime(auth['expires_at'][:19], '%Y-%m-%dT%H:%M:%S')) - time.timezone:
                continue
            auth['refresh_token'] = 'smoke-test-no-refresh'
            claude.append(source.parent.name)
        else:
            claims = json.loads(base64.urlsafe_b64decode(auth['tokens']['access_token'].split('.')[1] + '==='))
            if claims['exp'] < time.time() + 600:
                continue
            auth['tokens']['refresh_token'] = 'smoke-test-no-refresh'
            codex.append(source.parent.name)
        (RUN / 'data' / source.parent.name).mkdir()
        (RUN / 'data' / source.parent.name / 'auth.json').write_text(json.dumps(auth))
    if not codex or not claude:
        raise Failure(f'The smoke test needs a signed-in Codex and Claude account with ten minutes left on its token; found {codex} and {claude}.')
    CODEX_ACCOUNTS[:], CLAUDE_ACCOUNTS[:] = codex, claude
    (RUN / 'home/auth.json').write_text((RUN / 'data' / codex[0] / 'auth.json').read_text())


def fake_jwt(claims):
    encode = lambda value: base64.urlsafe_b64encode(json.dumps(value).encode()).decode().rstrip('=')
    return f'{encode({"alg": "none"})}.{encode(claims)}.{secrets.token_urlsafe(32)}'


def fake_codex_login(number):
    """A ChatGPT login only the fake providers accept, valid for a year."""
    auth = {'chatgpt_account_id': f'system-test-account-{number}', 'chatgpt_user_id': f'system-test-user-{number}', 'chatgpt_plan_type': 'pro'}
    claims = {'exp': int(time.time()) + 365 * 86400, 'email': f'codex-{number}@system-test.invalid', 'https://api.openai.com/auth': auth}
    return {'auth_mode': 'chatgpt', 'OPENAI_API_KEY': None, 'last_refresh': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()),
            'tokens': {'access_token': fake_jwt(claims), 'id_token': fake_jwt(claims), 'refresh_token': 'rt_' + secrets.token_urlsafe(32),
                       'account_id': auth['chatgpt_account_id']}}


def fake_claude_login(number):
    """A Claude login only the fake providers accept, valid for a year."""
    return {'provider': 'claude', 'access_token': 'sk-ant-oat01-' + secrets.token_urlsafe(32), 'refresh_token': 'sk-ant-ort01-' + secrets.token_urlsafe(32),
            'expires_at': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime(time.time() + 365 * 86400)),
            'account_id': f'system-test-claude-account-{number}', 'organization_id': f'system-test-claude-organization-{number}',
            'email': f'claude-{number}@system-test.invalid', 'plan': 'Max 20x'}


def start_fake_providers():
    """Starts the fake providers, which stand in for OpenAI and Anthropic."""
    log = (RUN / 'logs/fake-providers.log').open('w')
    started_processes.append(subprocess.Popen([str(RUN / 'fake-providers'), '--data-dir', str(RUN / 'data'), '--control', str(RUN / 'control'),
                                               '--certificate-authority', str(RUN / 'fake-providers-ca.pem'), '--requests', str(RUN / 'logs/requests')],
                                              stdout=log, stderr=log, start_new_session=True))
    origins = wait_for(lambda: re.search(r'(http://127\.0\.0\.1:\d+) (https://127\.0\.0\.1:\d+)', read('logs/fake-providers.log')),
                       'the fake providers to listen', timeout=30, interval=0.05)
    (RUN / 'fake-providers-origin').write_text(origins[1])
    (RUN / 'fake-providers-secure-origin').write_text(origins[2])


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
        # launchd waits ten seconds before restarting a job that exited; the outage outcome need not.
        'ThrottleInterval': 1,
        'RunAtLoad': True,
        'ExitTimeOut': 0,
        'StandardOutPath': str(RUN / 'logs/launchd.log'),
        'StandardErrorPath': str(RUN / 'logs/launchd.log'),
    }))
    for label in LABELS:
        subprocess.run(['launchctl', 'bootout', f'gui/{os.getuid()}/{label}'], capture_output=True)
    subprocess.run(['launchctl', 'bootstrap', f'gui/{os.getuid()}', str(plist)], check=True)
    wait_for(deploy_status, 'the proxy to serve (see data/proxy.log)', timeout=60, interval=0.05)
    # The first menu snapshot runs Codex's app-server in this Codex home, which creates Codex's state database. A
    # client started before that would race it.
    wait_for(menu_snapshot_served, 'the first menu snapshot', interval=0.05)
    base = f'http://{listen_address()}/client/{read("data/client-capability").strip()}/backend-api/codex'
    write_codex_config(RUN / 'home', base)
    (RUN / 'pi/models.json').write_text(json.dumps({'providers': {'openai-codex': {'baseUrl': base, 'apiKey': PI_PLACEHOLDER_KEY}}}))
    return base


# Codex always reads the machine's own configuration as well as its home's, and a release build has no way to skip it.
SYSTEM_CODEX_CONFIG = Path('/etc/codex/config.toml')


def write_codex_config(home, base):
    """Points the Codex client in home at the proxy whose Codex URL is base, with nothing that reaches outside the test:
    no plugin sync from GitHub and none of the MCP servers the machine's own Codex configuration starts."""
    # Everything else Codex asks ChatGPT for, such as its apps connector, goes to the fake providers too.
    chatgpt = '' if REAL_SERVICES else f'chatgpt_base_url = "{read("fake-providers-secure-origin")}/backend-api/"\n'
    system = tomllib.loads(SYSTEM_CODEX_CONFIG.read_text()) if SYSTEM_CODEX_CONFIG.exists() else {}
    no_system_mcp = ''.join(f'[mcp_servers.{json.dumps(name)}]\nenabled = false\n' for name in system.get('mcp_servers', {}))
    (home / 'config.toml').write_text(
        f'model_provider = "switch-test"\nmodel = "{MODEL}"\nmodel_reasoning_effort = "low"\n' + chatgpt +
        'cli_auth_credentials_store = "file"\n[model_providers.switch-test]\nname = "OpenAI"\n'
        f'base_url = "{base}"\nwire_api = "responses"\nrequires_openai_auth = true\n'
        'supports_websockets = true\n[analytics]\nenabled = false\n[features]\nplugins = false\n' + no_system_mcp)


class OtherMac:
    """A second proxy, built from this tree, as on Josh's other Mac: its own data directory, Codex home and launchd job,
    signed in to the same Codex accounts against the same fake providers. It signed in to them in the opposite order,
    so its folders name each account differently: account-1 here is account-3 on the first proxy."""

    def __init__(self):
        self.data, self.home = OTHER_MAC / 'data', OTHER_MAC / 'home'
        for part in (self.data, self.home, self.home / 'Library/LaunchAgents'):
            part.mkdir(parents=True)
        for number, account in enumerate(reversed(CODEX_ACCOUNTS), 1):
            (self.data / f'account-{number}').mkdir()
            shutil.copy2(RUN / 'data' / account / 'auth.json', self.data / f'account-{number}/auth.json')
        shutil.copy2(RUN / 'home/auth.json', self.home / 'auth.json')
        with socket.socket() as reservation:
            reservation.bind(('127.0.0.1', 0))
            self.listen = f'127.0.0.1:{reservation.getsockname()[1]}'
        plist = self.home / 'Library/LaunchAgents' / f'{OTHER_MAC_LABEL}.plist'
        plist.write_bytes(plistlib.dumps({
            'Label': OTHER_MAC_LABEL,
            'ProgramArguments': [str(RUN / 'cli'), 'serve', '--listen', self.listen],
            'EnvironmentVariables': dict(proxy_environment(), CODEX_SWITCH_DATA_DIR=str(self.data), CODEX_HOME=str(self.home),
                                         PI_CODING_AGENT_DIR=str(OTHER_MAC / 'pi')),
            'KeepAlive': True, 'ThrottleInterval': 1, 'RunAtLoad': True, 'ExitTimeOut': 0,
            'StandardOutPath': str(RUN / 'logs/other-mac-launchd.log'), 'StandardErrorPath': str(RUN / 'logs/other-mac-launchd.log'),
        }))
        subprocess.run(['launchctl', 'bootout', f'gui/{os.getuid()}/{OTHER_MAC_LABEL}'], capture_output=True)
        subprocess.run(['launchctl', 'bootstrap', f'gui/{os.getuid()}', str(plist)], check=True)
        wait_for(lambda: menu_snapshot_served(self.data), "the other Mac's first menu snapshot", timeout=60, interval=0.05)
        write_codex_config(self.home, f'http://{self.listen}/client/{(self.data / "client-capability").read_text().strip()}/backend-api/codex')

    def codex_task(self, name, prompt):
        """Starts a `codex exec` task through this proxy."""
        return CodexTask(name, prompt, env=dict(client_environment(), CODEX_HOME=str(self.home)))

    def proxy_log(self):
        return (self.data / 'proxy.log').read_text()

    def pinned_account_key(self, task):
        with sqlite3.connect(f'file:{self.data / "codex-switch.db"}?mode=ro', uri=True) as database:
            row = database.execute('SELECT account_key FROM task_account_pins WHERE codex_task_id = ?', (task,)).fetchone()
        return row[0] if row else None


def proxy_environment():
    # PATH lets the proxy find a Codex CLI installed without the desktop app.
    environment = {'CODEX_SWITCH_DATA_DIR': str(RUN / 'data'), 'CODEX_HOME': str(RUN / 'home'), 'PI_CODING_AGENT_DIR': str(RUN / 'pi'),
                   'PATH': os.environ['PATH']}
    if not REAL_SERVICES:
        environment.update({'CODEX_SWITCH_UPSTREAM_ORIGIN': read('fake-providers-origin'), 'CODEX_SWITCH_CLAUDE_UPSTREAM_ORIGIN': read('fake-providers-origin'),
                            'CODEX_SWITCH_CLAUDE_TOKEN_ORIGIN': read('fake-providers-origin'),
                            'CODEX_SWITCH_ACCOUNT_READ_SECONDS': str(ACCOUNT_READ_SECONDS),
                            'CODEX_SWITCH_CONNECTION_DRAIN_SECONDS': str(CONNECTION_DRAIN_SECONDS),
                            'CODEX_SWITCH_MENU_SNAPSHOT_SECONDS': str(MENU_SNAPSHOT_SECONDS)})
    return environment


def client_environment():
    # A proxy from the environment, such as the live Codex Switch an agent runs in, would carry the clients' requests.
    environment = {name: value for name, value in os.environ.items() if not name.startswith(('CODEX_', 'OPENAI_', 'PI_'))
                   and name.lower() not in ('https_proxy', 'http_proxy', 'all_proxy', 'no_proxy', 'node_extra_ca_certs')}
    environment.update(CODEX_HOME=str(RUN / 'home'), PI_CODING_AGENT_DIR=str(RUN / 'pi'))
    if not REAL_SERVICES:
        # Codex trusts only the fake providers' certificate authority, so none of its requests reach a real service.
        environment['SSL_CERT_FILE'] = str(RUN / 'fake-providers-ca.pem')
    return environment


def clean_up():
    """Stops every client, proxy generation and the fake providers; returns anything still running."""
    for name in (signal.SIGINT, signal.SIGTERM):
        signal.signal(name, signal.SIG_IGN)  # a second interrupt must not stop the cleanup
    for tunnel in started_tunnels:
        tunnel.close()
    fake_providers = str(RUN / 'fake-providers')
    if (RUN / 'control').exists():
        control()  # release held responses so draining processes can finish
    for process in started_processes:
        if process.args[0] != fake_providers:
            stop_process_group(process)
    for label in LABELS:
        subprocess.run(['launchctl', 'bootout', f'gui/{os.getuid()}/{label}'], capture_output=True)
    for pid in run_processes(exclude=fake_providers):
        try:
            os.kill(pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
    wait_for(lambda: not run_processes(exclude=fake_providers), 'proxy processes to stop', timeout=60, fail=False)
    for process in started_processes:
        stop_process_group(process)
    # Anything else from this run's directory, such as the fake providers of a group that ended without cleaning up.
    for pid in run_processes():
        try:
            os.kill(pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
    wait_for(lambda: not run_processes(), 'the run to stop', timeout=10, fail=False)
    leftovers = [f'process {pid}' for pid in run_processes()]
    for label in LABELS:
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


def run_processes(exclude=None, only_servers=False):
    """Processes running an executable from this run's directory."""
    listing = subprocess.run(['ps', '-axo', 'pid=,stat=,command='], capture_output=True, text=True, check=True).stdout
    owned = []
    for line in listing.splitlines():
        pid, state, command = line.split(None, 2)
        if (command.startswith(str(RUN) + '/') and not state.startswith('Z')
                and not (exclude and command.startswith(exclude)) and (not only_servers or ' serve ' in command)):
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

    def __init__(self, name, prompt, thread=None, env=None):
        self.name = name
        command = [CODEX, 'exec'] + (['resume', thread] if thread else ['--sandbox', 'workspace-write'])
        self.process = start_client(command + ['--json', '--skip-git-repo-check', prompt], name, env=env or client_environment())

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


def run_claude(session, must_succeed=True, resume=False, fast_mode=False):
    """Claude Code through the two printed proxy settings, to the fake providers (to Anthropic in the smoke test).
    resume continues the session, as a person's next prompt in it does. fast_mode asks for Opus in fast mode, as /fast does;
    the fake providers refuse it as Anthropic does without usage credits."""
    environment = {name: value for name, value in client_environment().items()
                   if not name.upper().startswith(('CLAUDE_', 'ANTHROPIC_'))
                   and not name.upper().endswith('_PROXY')
                   and name not in ('NODE_EXTRA_CA_CERTS', 'NODE_TLS_REJECT_UNAUTHORIZED')}
    environment.update(HOME=str(RUN / 'claude'), CLAUDE_CONFIG_DIR=str(RUN / 'claude'),
                       CODEX_SWITCH_DATA_DIR=str(RUN / 'data'))
    settings = subprocess.run([str(RUN / 'cli'), 'print-claude-config', '--listen', listen_address()],
                              env=environment, capture_output=True, text=True, check=True)
    environment.update(json.loads(settings.stdout)['env'])
    auth = json.loads((RUN / 'data' / CLAUDE_ACCOUNTS[0] / 'auth.json').read_text())
    environment['CLAUDE_CODE_OAUTH_TOKEN'] = auth['access_token']
    environment['CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC'] = '1'
    prompt, session_option = ('Reply with the word again only.', '--resume') if resume else ('Reply with the word ready only.', '--session-id')
    model = ['--model', 'opus', '--settings', json.dumps({'fastMode': True})] if fast_mode else ['--model', 'haiku']
    if fast_mode:
        # Claude Code asks Anthropic first and leaves fast mode off for an account without usage credits; T3's
        # Claude sessions on the Mac mini asked for it anyway, at the start of each turn.
        environment['CLAUDE_CODE_SKIP_FAST_MODE_ORG_CHECK'] = '1'
    process = start_client([CLAUDE, '-p', prompt, *model, '--effort', 'low',
                            '--output-format', 'json', session_option, session, '--tools', '',
                            '--setting-sources', '', '--strict-mcp-config'], 'claude', env=environment)
    if process.wait(timeout=180) and must_succeed:
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


class AppServer:
    """The official Codex app-server over stdio, as the Codex app drives it."""

    def __init__(self, name):
        self.name, self.notifications, self.responses, self.next_id = name, [], queue.Queue(), 0
        self.process = start_client([CODEX, 'app-server', '--stdio'], name, stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
        threading.Thread(target=self.read_messages, daemon=True).start()
        self.request('initialize', {'clientInfo': {'name': 'codex_switch_system_test', 'version': '1'}, 'capabilities': {'experimentalApi': True}})
        self.send({'method': 'initialized', 'params': {}})

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

    def close(self):
        self.process.stdin.close()
        self.process.wait(timeout=30)


def fork_codex_task(name, thread, prompt):
    """Forks a Codex task and runs one turn in the fork, as the Codex app does after a task's turns. Returns the fork's thread ID."""
    server = AppServer(name)
    fork = server.request('thread/fork', {'threadId': thread})['thread']['id']
    server.request('turn/start', {'threadId': fork, 'input': [{'type': 'text', 'text': prompt}]})
    wait_for(lambda: any(message.get('method') == 'turn/completed' for message in server.notifications), f'the fork {name} to finish its turn', timeout=120)
    server.close()
    return fork


class Goal(AppServer):
    """One /goal in the official Codex app-server, started once and then left to run by itself."""

    def __init__(self, name, objective):
        super().__init__(name)
        self.effect_file, self.result_file = RUN / f'work/{name}-effects.txt', RUN / f'work/{name}-result.txt'
        self.thread = self.request('thread/start', {'cwd': str(RUN / 'work'), 'model': MODEL, 'sandbox': 'workspace-write',
                                                    'approvalPolicy': 'never'})['thread']['id']
        objective = objective.format(effect=self.effect_file.name, result=self.result_file.name, release=f'{name}-release')
        # As the Codex app sends /goal. Codex starts the goal's first turn itself, with the objective.
        self.request('thread/goal/set', {'threadId': self.thread, 'origin': 'user', 'objective': objective, 'status': 'active', 'tokenBudget': 200000})

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
        self.close()
        return [message for message in self.notifications if message.get('method') == 'error']


class TunnelPair:
    """Two CONNECT tunnels to an echo server, one transferring and one idle."""
    def __init__(self):
        class Echo(socketserver.BaseRequestHandler):
            def handle(self):
                try:
                    while data := self.request.recv(4096):
                        self.request.sendall(data)
                except ConnectionError:
                    pass  # the proxy closed this tunnel
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
                self.stopped.wait(0.2)  # well inside the idle grace, so the tunnel counts as transferring
        except OSError as error:
            self.error = error

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


def account_key(folder):
    """The key the proxy records an account under, from the login saved in its folder: its provider and email."""
    path = RUN / 'data' / folder / 'auth.json'
    if not path.exists():
        return None
    auth = json.loads(path.read_text())
    if auth.get('provider') == 'claude':
        return 'claude:' + auth['email'].lower()
    claims = json.loads(base64.urlsafe_b64decode(auth['tokens']['id_token'].split('.')[1] + '==='))
    return 'chatgpt:' + claims['email'].lower()


T3_USAGE_URLS = {'codex': 'https://chatgpt.com/backend-api/wham/usage', 'claude': 'https://api.anthropic.com/api/oauth/usage'}


def t3_request(path, payload=None, key=None):
    """A request to the serving proxy's management API, as T3 makes it."""
    key = key or read('data/client-capability').strip()
    body = None if payload is None else json.dumps(payload).encode()
    headers = {'Authorization': 'Bearer ' + key, 'Content-Type': 'application/json'}
    request = urllib.request.Request(f'http://{listen_address()}/v0/management/{path}', body, headers)
    with urllib.request.urlopen(request, timeout=5) as response:
        return json.load(response)


def t3_account_indexes():
    """The index T3 is handed for each account, with its provider: {auth_index: provider}."""
    return {file['auth_index']: file['provider'] for file in t3_request('auth-files')['files']}


def t3_call(auth_index, url, method='GET'):
    return t3_request('api-call', {'auth_index': auth_index, 'method': method, 'url': url, 'header': {'Authorization': 'Bearer $TOKEN$'}})


def t3_usage_failures(indexes):
    """Reads each account's usage by an index T3 holds, as T3 does; returns the indexes the serving proxy refused."""
    failures = {}
    for index, provider in indexes.items():
        response = t3_call(index, T3_USAGE_URLS[provider])
        if response['status_code'] != 200:
            failures[index] = f'{response["status_code"]} {response["body"]}'
    return failures


ACCOUNT_TABLES = ('task_account_pins', 'account_reported_resets', 'completed_responses', 'model_request_counts', 'weekly_allowance_readings')


def account_records():
    """Every record of an account in the proxy's database, as {(table, primary key): row}, each row a dictionary of its
    columns. A record's identity is its primary key, never its rowid: SQLite reuses the rowids of deleted rows. Its
    account_key is None where the record has none, and absent where only a release before account keys has written."""
    with sqlite3.connect(f'file:{RUN / "data/codex-switch.db"}?mode=ro', uri=True) as database:
        database.row_factory = sqlite3.Row
        records = {}
        for table in ACCOUNT_TABLES:
            columns = database.execute(f'PRAGMA table_info({table})').fetchall()
            primary_key = [column['name'] for column in sorted(columns, key=lambda column: column['pk']) if column['pk']]
            for row in database.execute(f'SELECT * FROM {table}'):
                records[(table, tuple(row[name] for name in primary_key))] = dict(row)
        return records


def database_rows(task):
    """Completed responses the proxy recorded for a task, oldest first, each with the folder of its account. The folder
    decides a record's account: builds before account keys know only folders."""
    with sqlite3.connect(f'file:{RUN / "data/codex-switch.db"}?mode=ro', uri=True) as database:
        return database.execute('SELECT account_name, input_tokens, cached_input_tokens, output_tokens, reasoning_tokens '
                                'FROM completed_responses WHERE codex_task_id = ? ORDER BY completed_at_unix_nano', (task,)).fetchall()


def database_row_count():
    with sqlite3.connect(f'file:{RUN / "data/codex-switch.db"}?mode=ro', uri=True) as database:
        return database.execute('SELECT count(*) FROM completed_responses').fetchone()[0]


def pinned_account(task):
    """The folder of the account the task is pinned to. The folder decides: the published release moves a pin by
    changing its folder alone."""
    with sqlite3.connect(f'file:{RUN / "data/codex-switch.db"}?mode=ro', uri=True) as database:
        row = database.execute('SELECT account_name FROM task_account_pins WHERE codex_task_id = ?', (task,)).fetchone()
    return row[0] if row else None


def fake_provider_events(event, **only):
    """Gateway log lines for one event, as dictionaries, optionally only those with the given field values."""
    found = []
    for line in read('logs/fake-providers.log').splitlines():
        fields = {name: value.strip('"') for name, value in re.findall(r'(\w+)=("[^"]*"|\S+)', line)}
        if fields.get('event') == event and all(fields.get(name) == value for name, value in only.items()):
            found.append(fields)
    return found


def fake_provider_event_times(event, **only):
    """When the fake providers logged each such event, in seconds since midnight."""
    times = []
    for line in read('logs/fake-providers.log').splitlines():
        fields = {name: value.strip('"') for name, value in re.findall(r'(\w+)=("[^"]*"|\S+)', line)}
        if fields.get('event') == event and all(fields.get(name) == value for name, value in only.items()):
            hours, minutes, seconds = line.split(' ', 1)[0].split(':')
            times.append(int(hours) * 3600 + int(minutes) * 60 + float(seconds))
    return times


def forwarded_requests(**only):
    """(account, request fingerprint) for every OpenAI model request the fake providers received, in order."""
    return [(event['account'], event['request']) for event in fake_provider_events('forwarded', **only)]


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


def cli_path():
    command = RUN / 'home/.local/bin/codex-switch'
    return command if command.exists() or command.is_symlink() else RUN / 'cli'


def proxy_command(*arguments):
    """Runs a person-facing command against this run's isolated account data."""
    environment = dict(client_environment(), CODEX_SWITCH_DATA_DIR=str(RUN / 'data'))
    result = subprocess.run([str(cli_path()), *arguments], env=environment, capture_output=True, text=True)
    if result.returncode:
        raise Failure(f'codex-switch {" ".join(arguments)} failed: {result.stderr.strip()}')
    return result.stdout


def account_keys_by_display_name():
    return dict(line.split('\t')[:2] for line in proxy_command('accounts').splitlines() if '\t' in line)


def usage_reads(account, used):
    """How many usage reads the fake providers answered for an account with this weekly level."""
    return len([event for event in fake_provider_events('usage-read', account=account) if float(event['used']) == used])


def proxy_log():
    return read('data/proxy.log')


# The proxy removes a credential from what it logs and shows wherever this many of its characters, from its start,
# appear. No real credential is shorter, and fewer are at most its public start, such as "sk-ant-oat01-" or a JWT header.
CREDENTIAL_PIECE = 16


def held_credentials():
    """Every credential in this run's saved logins, on every proxy, by where it is saved."""
    found = {}
    for path in RUN.glob('**/auth.json'):
        try:
            auth = json.loads(path.read_text())
        except (OSError, ValueError):
            continue
        tokens = auth.get('tokens') or auth
        for name in ('access_token', 'refresh_token', 'id_token'):
            if isinstance(tokens.get(name), str) and len(tokens[name]) >= CREDENTIAL_PIECE:
                found[tokens[name]] = f'the {name} in {path.relative_to(RUN)}'
    return found


def credential_pieces(text, credentials):
    """(line, credential) for each line of text that holds a part of a credential: CREDENTIAL_PIECE characters of it."""
    pieces = {credential[start:start + CREDENTIAL_PIECE]: credential for credential in credentials
              for start in range(len(credential) - CREDENTIAL_PIECE + 1)}
    found = []
    for line in text.splitlines():
        if any(line[start:start + CREDENTIAL_PIECE] in pieces for start in range(len(line) - CREDENTIAL_PIECE + 1)):
            # Tokens can share a start, such as a JWT header: name the one the line holds most of.
            found.append((line, max(credentials, key=lambda credential: sum(
                credential[start:start + CREDENTIAL_PIECE] in line for start in range(len(credential) - CREDENTIAL_PIECE + 1)))))
    return found


def proxy_output_files():
    """Every proxy log and launchd output file in this run, on every proxy."""
    return sorted({*RUN.glob('**/proxy.log'), *RUN.glob('**/*launchd.log')})


def menu_snapshot_served(data=RUN / 'data'):
    try:
        with urllib.request.urlopen((data / 'menu-snapshot-url').read_text().strip(), timeout=5) as response:
            return response.status == 200
    except (OSError, ValueError):
        return False


def deploy_status():
    try:
        capability = read('data/client-capability').strip()
        with urllib.request.urlopen(f'http://{listen_address()}/client/{capability}/codex-switch/deploy-status', timeout=5) as response:
            return json.load(response)
    except (OSError, ValueError):
        return None


def fake_provider_connections(pid):
    """The local addresses of a proxy process's open connections to the fake providers, as they log them in `remote`."""
    port = read('fake-providers-origin').rsplit(':', 1)[1]
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
    """Replaces the fake providers' simulation file in one step; no lines means every account is available."""
    (RUN / 'control.next').write_text(''.join(line + '\n' for line in lines))
    os.replace(RUN / 'control.next', RUN / 'control')


def wait_for(probe, what, timeout=60, fail=True, interval=0.1):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = probe()
        if value:
            return value
        time.sleep(interval)
    if fail:
        raise Failure(f'Timed out after {timeout} seconds waiting for {what}.')
    return None
