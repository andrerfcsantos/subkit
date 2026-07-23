package app

import (
	"bytes"
	"testing"
)

func TestCheckVideoErrorsCommandIsHidden(t *testing.T) {
	root := NewRootCommand()
	command, _, err := root.Find([]string{"check-video-errors"})
	if err != nil {
		t.Fatal(err)
	}
	if !command.Hidden {
		t.Fatal("check-video-errors command should be hidden")
	}
	var help bytes.Buffer
	root.SetOut(&help)
	if err := root.Help(); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(help.Bytes(), []byte("check-video-errors")) {
		t.Fatal("hidden command appears in root help")
	}
}
