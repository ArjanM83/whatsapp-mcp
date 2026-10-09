package main

import (
	"testing"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"google.golang.org/protobuf/proto"
)

func TestExtractTextContentCaptions(t *testing.T) {
	img := &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String("Vader en moeder van opa Meulendijks")}}
	if got := extractTextContent(img); got != "Vader en moeder van opa Meulendijks" {
		t.Errorf("image caption: got %q", got)
	}

	vid := &waProto.Message{VideoMessage: &waProto.VideoMessage{Caption: proto.String("filmpje")}}
	if got := extractTextContent(vid); got != "filmpje" {
		t.Errorf("video caption: got %q", got)
	}

	doc := &waProto.Message{DocumentWithCaptionMessage: &waProto.FutureProofMessage{
		Message: &waProto.Message{DocumentMessage: &waProto.DocumentMessage{Caption: proto.String("de offerte")}},
	}}
	if got := extractTextContent(doc); got != "de offerte" {
		t.Errorf("document with caption: got %q", got)
	}

	plain := &waProto.Message{Conversation: proto.String("gewone tekst")}
	if got := extractTextContent(plain); got != "gewone tekst" {
		t.Errorf("plain text regression: got %q", got)
	}

	none := &waProto.Message{ImageMessage: &waProto.ImageMessage{}}
	if got := extractTextContent(none); got != "" {
		t.Errorf("image without caption: got %q", got)
	}
}
