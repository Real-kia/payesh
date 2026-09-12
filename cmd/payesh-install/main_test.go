package main

import "testing"

func TestDigestFlagsRequireUniqueLowercaseSHA256(t *testing.T) {
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	parsed, err := (digestFlags{"payesh=" + digest}).parse()
	if err != nil || parsed["payesh"] != digest {
		t.Fatalf("parsed=%v err=%v", parsed, err)
	}
	for _, values := range []digestFlags{
		{"payesh=short"},
		{"payesh=ABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCD"},
		{"payesh=" + digest, "payesh=" + digest},
	} {
		if _, err := values.parse(); err == nil {
			t.Fatalf("invalid digest flags accepted: %v", values)
		}
	}
}
