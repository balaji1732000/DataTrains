package httpapi

import "testing"

func TestSafeArtifactPath(t *testing.T) {
	for _, value := range []string{"trajectory.json", "video/000001.mp4", "outputs/final image.png"} {
		if !safeArtifactPath(value) {
			t.Errorf("safeArtifactPath(%q) = false", value)
		}
	}
	for _, value := range []string{"", ".", "/absolute", "../escape", "video/../escape", "video\\escape"} {
		if safeArtifactPath(value) {
			t.Errorf("safeArtifactPath(%q) = true", value)
		}
	}
}

func TestParseByteRange(t *testing.T) {
	tests := []struct {
		value       string
		start, end  int64
		partial     bool
		shouldError bool
	}{
		{"", 0, 9, false, false},
		{"bytes=2-5", 2, 5, true, false},
		{"bytes=7-", 7, 9, true, false},
		{"bytes=-3", 7, 9, true, false},
		{"bytes=10-", 0, 0, false, true},
		{"bytes=1-2,4-5", 0, 0, false, true},
	}
	for _, test := range tests {
		start, end, partial, err := parseByteRange(test.value, 10)
		if (err != nil) != test.shouldError {
			t.Errorf("parseByteRange(%q) error = %v", test.value, err)
			continue
		}
		if err == nil && (start != test.start || end != test.end || partial != test.partial) {
			t.Errorf("parseByteRange(%q) = %d,%d,%v", test.value, start, end, partial)
		}
	}
}
