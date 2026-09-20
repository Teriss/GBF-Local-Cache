//go:build !windows

package main

import "context"

func (a *App) startTray(context.Context) {}

func (a *App) stopTray() {}
