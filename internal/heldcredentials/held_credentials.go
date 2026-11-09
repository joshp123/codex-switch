// Package heldcredentials keeps every credential this process has held, so
// that no log line or status output carries one, whatever a provider sends
// back. Code that reads or receives a credential remembers it; the proxy log,
// the command line's output and the menu snapshot remove every remembered
// credential on the way out. Code that writes a log line or an error needs to
// do nothing.
package heldcredentials

import (
	"io"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// minimumLength is how many of a credential's characters, from its start,
// are removed wherever they appear. A quote shortened to fit a log line can
// cut a credential short; its start is removed all the same. No real
// credential is shorter, and fewer characters are at most its public start,
// such as "sk-ant-oat01-" or a JWT header.
const minimumLength = 16

const replacement = "[credential removed]"

var (
	mutex sync.RWMutex
	// byStart holds every remembered credential under its first
	// minimumLength characters. Credentials are kept for the life of the
	// process: a provider can quote a token after it has been refreshed.
	byStart = map[string][]string{}
)

// Remember adds credentials to those removed from output. Values shorter than
// minimumLength are not credentials and are ignored.
func Remember(credentials ...string) {
	mutex.Lock()
	defer mutex.Unlock()
	for _, credential := range credentials {
		if len(credential) < minimumLength {
			continue
		}
		start := credential[:minimumLength]
		known := false
		for _, held := range byStart[start] {
			known = known || held == credential
		}
		if !known {
			byStart[start] = append(byStart[start], credential)
		}
	}
}

// Remove replaces every remembered credential in text, and the start of one
// cut short, with "[credential removed]". Where credentials share a start,
// such as two JWTs from one issuer, the longest match is removed.
func Remove(text string) string {
	mutex.RLock()
	defer mutex.RUnlock()
	if len(byStart) == 0 {
		return text
	}
	var removed strings.Builder
	kept := 0
	for index := 0; index+minimumLength <= len(text); {
		longest := 0
		for _, credential := range byStart[text[index:index+minimumLength]] {
			longest = max(longest, sharedStartLength(text[index:], credential))
		}
		if longest == 0 {
			index++
			continue
		}
		removed.WriteString(text[kept:index])
		removed.WriteString(replacement)
		index += longest
		kept = index
	}
	if kept == 0 {
		return text
	}
	removed.WriteString(text[kept:])
	return removed.String()
}

func sharedStartLength(text, credential string) int {
	length := 0
	for length < len(text) && length < len(credential) && text[length] == credential[length] {
		length++
	}
	return length
}

// RemovingWriter writes to Output with every remembered credential removed.
// Each write must hold whole lines, as the standard logger's do, so that no
// credential is split across two writes.
type RemovingWriter struct{ Output io.Writer }

func (writer RemovingWriter) Write(text []byte) (int, error) {
	if _, err := io.WriteString(writer.Output, Remove(string(text))); err != nil {
		return 0, err
	}
	return len(text), nil
}

// RemoveFromMessage removes every remembered credential from each string in
// message, however deeply nested.
func RemoveFromMessage(message proto.Message) {
	removeFromFields(message.ProtoReflect())
}

func removeFromFields(message protoreflect.Message) {
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		switch {
		case field.IsList():
			list := value.List()
			for index := 0; index < list.Len(); index++ {
				if replaced, changed := removeFromValue(field, list.Get(index)); changed {
					list.Set(index, replaced)
				}
			}
		case field.IsMap():
			value.Map().Range(func(key protoreflect.MapKey, item protoreflect.Value) bool {
				if replaced, changed := removeFromValue(field.MapValue(), item); changed {
					value.Map().Set(key, replaced)
				}
				return true
			})
		default:
			if replaced, changed := removeFromValue(field, value); changed {
				message.Set(field, replaced)
			}
		}
		return true
	})
}

// removeFromValue returns a string with credentials removed, or recurses into
// a message in place.
func removeFromValue(field protoreflect.FieldDescriptor, value protoreflect.Value) (protoreflect.Value, bool) {
	switch field.Kind() {
	case protoreflect.StringKind:
		if removed := Remove(value.String()); removed != value.String() {
			return protoreflect.ValueOfString(removed), true
		}
	case protoreflect.MessageKind, protoreflect.GroupKind:
		removeFromFields(value.Message())
	}
	return value, false
}
