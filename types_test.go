package lebro

import "testing"

func TestMessageValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		message Message
		wantErr bool
	}{
		{name: "user", message: Message{Role: RoleUser, Content: "hello"}},
		{name: "tool with call ID", message: Message{Role: RoleTool, ToolCallID: "call_1"}},
		{name: "unknown role", message: Message{Role: "unknown"}, wantErr: true},
		{name: "tool without call ID", message: Message{Role: RoleTool}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.message.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
