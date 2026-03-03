package vai

import "testing"

func TestToolArgStringDecoder_DefaultKeys_ContentAcrossChunks(t *testing.T) {
	t.Parallel()

	d := NewToolArgStringDecoder(ToolArgStringDecoderOptions{})

	u1 := d.Push(`{"content":"hel`)
	if !u1.Found || u1.Key != "content" || u1.Full != "hel" || u1.Delta != "hel" {
		t.Fatalf("u1=%+v", u1)
	}

	u2 := d.Push(`lo"}`)
	if !u2.Found || u2.Key != "content" || u2.Full != "hello" || u2.Delta != "lo" {
		t.Fatalf("u2=%+v", u2)
	}
}

func TestToolArgStringDecoder_FallbackToMessageThenText(t *testing.T) {
	t.Parallel()

	d := NewToolArgStringDecoder(ToolArgStringDecoderOptions{})

	u1 := d.Push(`{"message":"hi"}`)
	if !u1.Found || u1.Key != "message" || u1.Full != "hi" || u1.Delta != "hi" {
		t.Fatalf("u1=%+v", u1)
	}

	d.Reset()
	u2 := d.Push(`{"text":"hello"}`)
	if !u2.Found || u2.Key != "text" || u2.Full != "hello" || u2.Delta != "hello" {
		t.Fatalf("u2=%+v", u2)
	}
}

func TestToolArgStringDecoder_LocksFirstDetectedKey(t *testing.T) {
	t.Parallel()

	d := NewToolArgStringDecoder(ToolArgStringDecoderOptions{})

	u1 := d.Push(`{"message":"he`)
	if !u1.Found || u1.Key != "message" || u1.Full != "he" {
		t.Fatalf("u1=%+v", u1)
	}
	if d.MatchedKey() != "message" {
		t.Fatalf("MatchedKey=%q, want %q", d.MatchedKey(), "message")
	}

	u2 := d.Push(`llo","content":"IGNORED"}`)
	if !u2.Found || u2.Key != "message" || u2.Full != "hello" || u2.Delta != "llo" {
		t.Fatalf("u2=%+v", u2)
	}
}

func TestToolArgStringDecoder_IgnoresNestedOrStringLiteralKeys(t *testing.T) {
	t.Parallel()

	d := NewToolArgStringDecoder(ToolArgStringDecoderOptions{})
	u := d.Push(`{"meta":{"content":"x"},"note":"literal \"content\":\"y\"","message":"ok"}`)
	if !u.Found || u.Key != "message" || u.Full != "ok" || u.Delta != "ok" {
		t.Fatalf("u=%+v", u)
	}
}

func TestToolArgStringDecoder_HandlesEscapesAcrossChunks(t *testing.T) {
	t.Parallel()

	d := NewToolArgStringDecoder(ToolArgStringDecoderOptions{})
	u1 := d.Push(`{"content":"a\`)
	if !u1.Found || u1.Key != "content" || u1.Full != "a" || u1.Delta != "a" {
		t.Fatalf("u1=%+v", u1)
	}

	u2 := d.Push(`n b \"q`)
	if !u2.Found || u2.Key != "content" || u2.Full != "a\n b \"q" || u2.Delta != "\n b \"q" {
		t.Fatalf("u2=%+v", u2)
	}

	u3 := d.Push(`\""}`)
	if !u3.Found || u3.Key != "content" || u3.Full != "a\n b \"q\"" || u3.Delta != "\"" {
		t.Fatalf("u3=%+v", u3)
	}
}

func TestToolArgStringDecoder_HandlesUnicodeAndSurrogatesAcrossChunks(t *testing.T) {
	t.Parallel()

	d := NewToolArgStringDecoder(ToolArgStringDecoderOptions{})

	u1 := d.Push(`{"content":"face \uD83`)
	if !u1.Found || u1.Key != "content" || u1.Full != "face " || u1.Delta != "face " {
		t.Fatalf("u1=%+v", u1)
	}

	u2 := d.Push(`D\uDE`)
	if !u2.Found || u2.Key != "content" || u2.Full != "face " || u2.Delta != "" {
		t.Fatalf("u2=%+v", u2)
	}

	u3 := d.Push(`00"}`)
	if !u3.Found || u3.Key != "content" || u3.Full != "face 😀" || u3.Delta != "😀" {
		t.Fatalf("u3=%+v", u3)
	}
}

func TestToolArgStringDecoder_NonStringCandidateValue(t *testing.T) {
	t.Parallel()

	d := NewToolArgStringDecoder(ToolArgStringDecoderOptions{})
	u := d.Push(`{"content":123,"message":"hi"}`)
	if !u.Found || u.Key != "message" || u.Full != "hi" || u.Delta != "hi" {
		t.Fatalf("u=%+v", u)
	}
}

func TestToolArgStringDecoder_Reset(t *testing.T) {
	t.Parallel()

	d := NewToolArgStringDecoder(ToolArgStringDecoderOptions{})
	u1 := d.Push(`{"content":"hello"}`)
	if !u1.Found || u1.Key != "content" || u1.Full != "hello" || u1.Delta != "hello" {
		t.Fatalf("u1=%+v", u1)
	}

	d.Reset()
	if d.MatchedKey() != "" {
		t.Fatalf("MatchedKey after reset=%q, want empty", d.MatchedKey())
	}

	u2 := d.Push(`{"content":"bye"}`)
	if !u2.Found || u2.Key != "content" || u2.Full != "bye" || u2.Delta != "bye" {
		t.Fatalf("u2=%+v", u2)
	}
}
