package main

import (
	"log"
	"os"
	"time"

	"github.com/fsnotify/fsnotify"
)

func StartWatcher(dir string, state *State, reconcileCh chan<- struct{}, logger *log.Logger) error {
	if err := syncDirectoryState(dir, state); err != nil {
		return err
	}
	sendReconcile(reconcileCh)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	if err := watcher.Add(dir); err != nil {
		return err
	}

	go func() {
		defer watcher.Close()
		var timer *time.Timer
		events := make(chan struct{}, 1)

		enqueue := func() {
			select {
			case events <- struct{}{}:
			default:
			}
		}

		go func() {
			for range events {
				if timer != nil {
					timer.Stop()
				}
				timer = time.AfterFunc(100*time.Millisecond, func() {
					if err := syncDirectoryState(dir, state); err != nil {
						logger.Printf("failed to sync directory state: %v", err)
						return
					}
					sendReconcile(reconcileCh)
				})
			}
		}()

		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if shouldHandleEvent(event) {
					enqueue()
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				logger.Printf("watcher error: %v", err)
			}
		}
	}()

	return nil
}

func shouldHandleEvent(event fsnotify.Event) bool {
	if event.Op&(fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
		return false
	}
	return true
}

func syncDirectoryState(dir string, state *State) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	found := map[string]bool{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		site := entry.Name()
		found[site] = true
		state.AddSite(site)
	}

	for _, site := range state.AllSiteNames() {
		if !found[site] {
			state.RemoveSite(site)
		}
	}
	return nil
}

func sendReconcile(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
