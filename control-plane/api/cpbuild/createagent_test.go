package cpbuild

import (
	"reflect"
	"testing"
)

// TestChannelNames: an empty/blank list becomes the default freehold channel;
// blanks are dropped, duplicates collapsed preserving order, and #freehold is
// always present — every agent joins the shared workroom (case-insensitively).
func TestChannelNames(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want []string
	}{
		{nil, []string{"#freehold"}},
		{[]string{"", "  "}, []string{"#freehold"}},
		{[]string{"#a", "#a", "#b"}, []string{"#a", "#b", "#freehold"}},
		{[]string{"#freehold", "#vault"}, []string{"#freehold", "#vault"}},
		{[]string{"#kitchen"}, []string{"#kitchen", "#freehold"}},
		{[]string{"#Kitchen", "#kitchen"}, []string{"#Kitchen", "#freehold"}},
	} {
		if got := channelNames(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("channelNames(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
