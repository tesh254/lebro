package openai

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/tesh254/lebro"
)

// mapToolNames supplies provider-safe names while retaining the canonical
// tool IDs for execution and persisted history.
func mapToolNames(request lebro.ModelRequest) (lebro.ModelRequest, map[string]lebro.ToolID, error) {
	ids := make(map[lebro.ToolID]struct{})
	for _, tool := range request.Tools {
		ids[tool.ID] = struct{}{}
	}
	for _, message := range request.Messages {
		for _, call := range message.ToolCalls.Values() {
			ids[call.ToolID] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(ids))
	used := make(map[string]bool)
	for id := range ids {
		ordered = append(ordered, string(id))
		if validToolName(string(id)) {
			used[string(id)] = true
		}
	}
	sort.Strings(ordered)
	forward := make(map[lebro.ToolID]string, len(ids))
	reverse := make(map[string]lebro.ToolID, len(ids))
	for _, name := range ordered {
		alias := name
		if !validToolName(name) {
			sum := sha256.Sum256([]byte(name))
			var label strings.Builder
			for _, c := range name {
				if label.Len() >= 32 {
					break
				}
				if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
					label.WriteRune(c)
				} else {
					label.WriteByte('_')
				}
			}
			for counter := 0; ; counter++ {
				if counter == 0 {
					alias = fmt.Sprintf("lebro_%s_%x", label.String(), sum[:10])
				} else {
					alias = fmt.Sprintf("lebro_%x_%d", sum[:24], counter)
				}
				if !used[alias] {
					break
				}
			}
			used[alias] = true
		}
		forward[lebro.ToolID(name)] = alias
		reverse[alias] = lebro.ToolID(name)
	}
	request.Tools = append([]lebro.ToolDefinition(nil), request.Tools...)
	for i := range request.Tools {
		request.Tools[i].ID = lebro.ToolID(forward[request.Tools[i].ID])
	}
	request.Messages = append([]lebro.Message(nil), request.Messages...)
	for i := range request.Messages {
		calls := request.Messages[i].ToolCalls.Values()
		if len(calls) == 0 {
			continue
		}
		for j := range calls {
			calls[j].ToolID = lebro.ToolID(forward[calls[j].ToolID])
		}
		encoded, err := lebro.NewModelToolCalls(calls...)
		if err != nil {
			return request, nil, err
		}
		request.Messages[i].ToolCalls = encoded
	}
	// Preserve an explicitly selected function when its canonical ID needs
	// a provider-safe alias. Other extension values remain untouched.
	var extension map[string]json.RawMessage
	if json.Unmarshal(request.Extension, &extension) == nil {
		var choice map[string]json.RawMessage
		if json.Unmarshal(extension["tool_choice"], &choice) == nil {
			var function map[string]json.RawMessage
			if json.Unmarshal(choice["function"], &function) == nil {
				var name string
				if json.Unmarshal(function["name"], &name) == nil {
					if alias, ok := forward[lebro.ToolID(name)]; ok && alias != name {
						function["name"], _ = json.Marshal(alias)
						choice["function"], _ = json.Marshal(function)
						extension["tool_choice"], _ = json.Marshal(choice)
						request.Extension, _ = json.Marshal(extension)
					}
				}
			}
		}
	}
	return request, reverse, nil
}

func validToolName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for _, c := range name {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

type toolNameReader struct {
	lebro.StreamReader
	names map[string]lebro.ToolID
}

func (r *toolNameReader) Next() (lebro.StreamDelta, error) {
	delta, err := r.StreamReader.Next()
	if delta.ToolCall != nil {
		if id, ok := r.names[string(delta.ToolCall.ToolID)]; ok {
			call := *delta.ToolCall
			call.ToolID = id
			delta.ToolCall = &call
		}
	}
	return delta, err
}
