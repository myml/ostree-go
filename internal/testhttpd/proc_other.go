//go:build !linux

package main

func setParentDeathSignal() {}

func newSession() error { return nil }

func redirectStdioToDevNull(int) {}
