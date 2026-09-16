package chathub

import "testing"

// A data URL that starts with "data:image/" but carries no comma has no payload.
// The check used strings.SplitN(s, ",", 2)[1], which is a panic on a
// one-element slice, and the value comes from upstream frames rather than from
// anything this package controls.
func TestIsImageURLToleratesCommaLessDataURL(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"data:image/png", false},        // no comma at all
		{"data:image/png;base64", false}, // no comma
		{"data:image/", false},
		{"data:image/png;base64,", true},       // comma, empty payload decodes fine
		{"data:image/png;base64,iVBOR", false}, // comma, invalid base64
		{"data:image/png;base64,aGk=", true},   // comma, valid base64
		{"", false},
		{"not a url", false},
	}
	for _, c := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("isImageURL(%q) panicked: %v", c.in, r)
				}
			}()
			if got := isImageURL(c.in); got != c.want {
				t.Errorf("isImageURL(%q) = %v, want %v", c.in, got, c.want)
			}
		}()
	}
}
