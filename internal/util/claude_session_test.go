package util

import "testing"

func TestExtractClaudeAccountSessionID(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "valid claude metadata user id",
			raw:  "user_04408135829d8e3beb46401215e3163fab6c5f9c857b575056e37708c9e3f91b_account__session_b77671ff-818b-4676-9aee-b6d6466cbd6c",
			want: "b77671ff-818b-4676-9aee-b6d6466cbd6c",
		},
		{
			name: "stops at invalid trailing rune",
			raw:  "user_x_account__session_B77671ff-818b-4676-9aee-b6d6466cbd6c}",
			want: "b77671ff-818b-4676-9aee-b6d6466cbd6c",
		},
		{
			name: "non claude format ignored",
			raw:  "user-123",
			want: "",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ExtractClaudeAccountSessionID(tc.raw); got != tc.want {
				t.Fatalf("ExtractClaudeAccountSessionID(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}
