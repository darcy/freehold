package cpbuild

import (
	"reflect"
	"testing"
)

// TestChannelNames: an empty/blank list becomes the default freehold channel;
// blanks are dropped and duplicates collapsed, preserving order.
func TestChannelNames(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want []string
	}{
		{nil, []string{"#freehold"}},
		{[]string{"", "  "}, []string{"#freehold"}},
		{[]string{"#a", "#a", "#b"}, []string{"#a", "#b"}},
		{[]string{"#freehold", "#vault"}, []string{"#freehold", "#vault"}},
	} {
		if got := channelNames(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("channelNames(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
