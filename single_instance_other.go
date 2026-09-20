//go:build !windows

package main

func acquireSingleInstance() (release func(), alreadyRunning bool, err error) {
	return func() {}, false, nil
}
