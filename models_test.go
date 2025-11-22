package acudp

import (
	"bytes"
	"testing"
)

func TestSanitizeString(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Hello World", "Hello World"},
		{"Héllo Wörld", "Hllo Wrld"}, // Non-ascii removed
		{"123!@#", "123!@#"},
	}

	for _, test := range tests {
		if got := sanitizeString(test.input); got != test.expected {
			t.Errorf("sanitizeString(%q) = %q, want %q", test.input, got, test.expected)
		}
	}
}

func TestEncodeUTF32(t *testing.T) {
	input := "ABC"
	// A = 65 (0x41), B = 66 (0x42), C = 67 (0x43)
	// Little Endian UTF-32:
	// 41 00 00 00
	// 42 00 00 00
	// 43 00 00 00
	expected := []byte{0x41, 0x00, 0x00, 0x00, 0x42, 0x00, 0x00, 0x00, 0x43, 0x00, 0x00, 0x00}

	got, err := encodeUTF32(input)
	if err != nil {
		t.Fatalf("encodeUTF32(%q) returned error: %v", input, err)
	}

	if !bytes.Equal(got, expected) {
		t.Errorf("encodeUTF32(%q) = %v, want %v", input, got, expected)
	}
}
