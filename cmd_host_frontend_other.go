//go:build !darwin && !linux

package main

func selectNativeFrontend() error { return nil }
