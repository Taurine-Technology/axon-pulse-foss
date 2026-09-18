//go:build !darwin && !linux

package main

func prepareDesktopRelaunch(string) (bool, error) { return false, nil }
