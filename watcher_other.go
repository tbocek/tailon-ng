//go:build !linux

package main

import "context"

// Platforms without inotify fall back to polling every pollInterval — the
// pre-notification behavior, with latency bounded by the interval.
type fileWatch struct{}

func watchFile(string) *fileWatch                  { return &fileWatch{} }
func (w *fileWatch) wait(ctx context.Context) bool { return sleep(ctx, pollInterval) }
func (w *fileWatch) rewatch()                      {}
func (w *fileWatch) close()                        {}
