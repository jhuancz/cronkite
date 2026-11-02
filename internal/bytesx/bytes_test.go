package bytesx

import "testing"

func TestParse(t *testing.T) {
	valid := map[string]int64{
		"1024":   1024,
		"1KiB":   1024,
		"1k":     1024,
		"500MB":  500 << 20,
		"5GiB":   5 << 30,
		"1.5MiB": 1572864,
		"0":      0,
	}
	for in, want := range valid {
		got, err := Parse(in)
		if err != nil {
			t.Errorf("Parse(%q) returned error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Parse(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, in := range []string{"", "abc", "-1GB", "12XB", "MB"} {
		if got, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %d, want an error", in, got)
		}
	}
}

func TestFormat(t *testing.T) {
	cases := map[int64]string{
		0:       "0 B",
		512:     "512 B",
		1024:    "1.0 KiB",
		1572864: "1.5 MiB",
		5 << 30: "5.0 GiB",
		1023:    "1023 B",
		-1024:   "-1.0 KiB",
	}
	for in, want := range cases {
		if got := Format(in); got != want {
			t.Errorf("Format(%d) = %q, want %q", in, got, want)
		}
	}
}
