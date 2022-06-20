package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"github.com/0leksandr/my.go"
	"github.com/fsnotify/fsnotify"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Watcher interface {
	io.Closer
	LoggerAware
	Modifications() <-chan Modification
	Name() string
}

type FsnotifyWatcher struct { // PRIORITY: watch new subdirectories
	Watcher
	modifications chan Modification
	stopWatching  func()
}
func (FsnotifyWatcher) New(root string, exclude string) Watcher {
	var ignored *regexp.Regexp
	if exclude != "" { ignored = regexp.MustCompile(exclude) }
	watcher := FsnotifyWatcher{
		modifications: make(chan Modification),
	}
	var events <-chan fsnotify.Event
	events, watcher.stopWatching = FsnotifyWatcher{}.watchDirRecursive(root, ignored)

	getPath := func(event fsnotify.Event) Path {
		return Path{}.New(
			Filename(event.Name[len(root)+1:]),
			false, // TODO: implement
		)
	}

	var processEvent func(event fsnotify.Event)
	processEvent = func(event fsnotify.Event) {
		if event.Op == 0 { return } // MAYBE: report? This is weird
		if event.Op == fsnotify.Chmod { return }
		path := getPath(event)
		if ignored != nil && ignored.MatchString(path.original.Real()) { return }

		switch event.Op {
			case fsnotify.Create, fsnotify.Write:
				watcher.put(Updated{path})
			case fsnotify.Remove:
				watcher.put(Deleted{path})
			case fsnotify.Rename:
				putDefault := func() { watcher.put(Deleted{path}) }
				select {
					case nextEvent, ok := <-events:
						if ok {
							if nextEvent.Op == fsnotify.Create { // MAYBE: check contents (checksums, modification times)
								watcher.put(Moved{
									from: path,
									to:   getPath(nextEvent),
								})
							} else {
								putDefault()
								processEvent(nextEvent)
							}
						} else {
							putDefault()
						}
					case <-time.After(1 * time.Millisecond): // MAYBE: tweak
						putDefault()
					// MAYBE: listen for exit
				}
			default:
				panic("Unknown event")
		}
	}

	go func() {
		for event := range events {
			processEvent(event)
		}
	}()

	return &watcher
}
func (FsnotifyWatcher) Name() string {
	return "fsnotify"
}
func (watcher *FsnotifyWatcher) Close() error {
	watcher.stopWatching()
	close(watcher.modifications)
	return nil
}
func (watcher *FsnotifyWatcher) Modifications() <-chan Modification {
	return watcher.modifications
}
func (watcher *FsnotifyWatcher) SetLogger(Logger) {
	// MAYBE: implement
}
func (FsnotifyWatcher) watchDirRecursive(
	path string,
	ignored *regexp.Regexp,
) (<-chan fsnotify.Event, context.CancelFunc) {
	watcher, err := fsnotify.NewWatcher()
	PanicIf(err)

	isIgnored := func(path2 string) bool {
		if ignored == nil { return false }
		if path2[:len(path)] != path {
			panic(fmt.Sprintf("Unexpected local path: %s", path2))
		}
		path2 = path2[len(path):]
		if path2 != "" {
			// TODO: platform-specific directory separators
			if path2[0] != '/' { panic(fmt.Sprintf("Unexpected local path: %s", path2)) }
			path2 = path2[1:]
		}
		return ignored.MatchString(path2)
	}

	Must(filepath.Walk(
		path,
		func(path string, fi os.FileInfo, err error) error {
			if isIgnored(path) { return nil }
			PanicIf(err)
			if fi.Mode().IsDir() { return watcher.Add(path) }
			return nil
		},
	))

	go func() {
		PanicIf(<-watcher.Errors) // MAYBE: return errors channel
	}()

	return watcher.Events, func() { Must(watcher.Close()) }
}
func (watcher *FsnotifyWatcher) put(modification Modification) { // MAYBE: remove
	watcher.modifications <- modification
}

type InotifyWatcher struct {
	Watcher
	modifications chan Modification
	logger        Logger
	onClose       func() error
}
func (InotifyWatcher) New(root string, exclude string, logger Logger) (Watcher, error) {
	modifications := make(chan Modification) // MAYBE: reserve size
	watcher := InotifyWatcher{
		modifications: modifications,
		logger:        logger,
		onClose: func() error {
			close(modifications)
			return nil
		},
	}

	nrFiles, errCalculateFiles := watcher.getNrFiles(root)
	if errCalculateFiles != nil { return &watcher, errCalculateFiles }
	maxUserWatchers, errMaxUserWatchers := watcher.getMaxUserWatchers()
	if errMaxUserWatchers != nil { return &watcher, errMaxUserWatchers }
	requiredNrWatchers := watcher.getRequiredNrWatchers(nrFiles)
	if requiredNrWatchers > maxUserWatchers { // THINK: https://www.baeldung.com/linux/inotify-upper-limit-reached
		if err := watcher.setMaxUserWatchers(requiredNrWatchers); err != nil { return &watcher, err }
	}

	const CloseWriteStr = "CLOSE_WRITE"
	const DeleteStr     = "DELETE"
	const MovedFromStr  = "MOVED_FROM"
	const MovedToStr    = "MOVED_TO"

	const IsDir = "ISDIR"

	type EventType uint8
	const (
		CloseWriteCode EventType = 1 << iota
		DeleteCode
		MovedFromCode
		MovedToCode
	)

	// something that never can be a part of a path/filename
	// MAYBE: something else for exotic filesystems.
	//        See https://en.wikipedia.org/wiki/Filename#Comparison_of_filename_limitations
	const Delimiter = "///"

	args := []string{
		"--monitor",
		"--recursive",
		"--format", strings.Join([]string{
			"%w%f",
			Delimiter,
			"%e",
		}, ""),
		"--event", CloseWriteStr,
		"--event", DeleteStr,
		"--event", MovedFromStr,
		"--event", MovedToStr,
	}
	if exclude != "" {
		args = append(args, "--exclude", exclude) // TODO: test
	}
	command := exec.Command("inotifywait", append(args, "--", root)...)

	type Event struct {
		eventType EventType
		path      Path
	}
	events := make(chan Event) // MAYBE: reserve size
	watcher.onClose = func() error {
		close(events)
		close(modifications)
		return nil
	}

	stdout, err1 := command.StdoutPipe()
	PanicIf(err1)
	stdoutScanner := bufio.NewScanner(stdout)
	reg := regexp.MustCompile(fmt.Sprintf(
		"(?s)^%s(.+)%s([A-Z_,]+)$",
		stripTrailSlash(root) + string(os.PathSeparator),
		Delimiter,
	))
	knownTypes := []struct {
		str  string
		code EventType
	}{
		{CloseWriteStr, CloseWriteCode},
		{DeleteStr,     DeleteCode    },
		{MovedFromStr,  MovedFromCode },
		{MovedToStr,    MovedToCode   },
	}
	go func() { // stdout to events
		for {
			line := func() string {
				var lines []string
				for stdoutScanner.Scan() {
					lines = append(lines, stdoutScanner.Text())
					line := lines[0]
					if len(lines) > 1 { line = strings.Join(lines, "\n") }
					if reg.MatchString(line) { return line }
				}
				return ""
			}()
			if line == "" { break }
			watcher.logger.Debug("inotify.line", line)
			parts := reg.FindStringSubmatch(line)
			path := Path{}.New(Filename(parts[1]), false)
			eventsStr := parts[2]
			eventTypes := strings.Split(eventsStr, ",")
			knownType, errReadEvent := func() (EventType, error) {
				for _, eventType := range eventTypes {
					for _, knownType := range knownTypes {
						if eventType == knownType.str { return knownType.code, nil }
					}
				}
				return 0, errors.New("unknown event: " + eventsStr)
			}()
			if errReadEvent == nil {
				for _, eventType := range eventTypes {
					if eventType == IsDir {
						path.isDir = true
						break
					}
				}
				events <- Event{
					eventType: knownType,
					path:      path,
				}
			} else {
				watcher.logger.Error(errReadEvent.Error())
			}
		}

		close(events)
	}()

	put := func(modification Modification) { watcher.modifications <- modification }
	var processEvent func(Event)
	processEvent = func(event Event) {
		watcher.logger.Debug("event", event)
		path := event.path
		switch event.eventType {
			case CloseWriteCode: put(Updated{path})
			case DeleteCode: put(Deleted{path})
			case MovedFromCode:
				putDefault := func() { put(Deleted{path}) }
				select {
					case nextEvent, ok := <- events:
						if ok {
							if nextEvent.eventType == MovedToCode {
								if path.isDir != nextEvent.path.isDir { panic("inconsistent move") } // MAYBE: some fallback
								put(Moved{
									from: path,
									to:   nextEvent.path,
								})
							} else {
								putDefault()
								processEvent(nextEvent)
							}
						} else {
							putDefault()
						}
					case <-time.After(2 * time.Millisecond): // MAYBE: tweak
						putDefault()
					// MAYBE: listen for exit
				}
			case MovedToCode: put(Updated{path})
		}
	}
	go func() { // events to modifications
		for event := range events { processEvent(event) }
		close(watcher.modifications)
		return
	}()

	errCommandStart := command.Start()
	watcher.onClose = func() error {
		return command.Process.Signal(syscall.SIGTERM)
	}

	// TODO: read error/info stream, await for watches to establish
	return &watcher, errCommandStart
}
func (InotifyWatcher) Name() string {
	return "inotify"
}
func (watcher *InotifyWatcher) Close() error {
	return watcher.onClose()
}
func (watcher *InotifyWatcher) Modifications() <-chan Modification {
	return watcher.modifications
}
func (watcher *InotifyWatcher) SetLogger(logger Logger) {
	watcher.logger = logger
}
func (watcher *InotifyWatcher) getNrFiles(root string) (uint64, error) {
	var nrFiles uint64
	done := Locker{}
	done.Lock()
	var errStopwatch error
	doNotStartStopwatch := cancellableTimer(
		1 * time.Second,
		func() {
			errStopwatch = stopwatch(
				"Calculating number of files in watched directory",
				func() error {
					done.Wait()
					return nil
				},
			)
			fmt.Println(fmt.Sprintf("%d files must be watched in total", nrFiles))
		},
	)
	my.RunCommand(
		root,
		"find . -type f |wc -l",
		func(out string) {
			var err error
			nrFiles, err = strconv.ParseUint(out, 10, 64)
			PanicIf(err)
		},
		WriteToStderr,
	)
	(*doNotStartStopwatch)()
	done.Unlock()
	if errStopwatch != nil { return 0, errStopwatch }
	return nrFiles, nil
}
func (watcher *InotifyWatcher) getMaxUserWatchers() (uint64, error) {
	var maxNrWatchers uint64
	var errParseUint, errCat error
	if !my.RunCommand(
		"",
		"cat /proc/sys/fs/inotify/max_user_watches",
		func(out string) {
			maxNrWatchers, errParseUint = strconv.ParseUint(out, 10, 64)
		},
		func(err string) {
			errCat = errors.New(err)
		},
	) {
		return 0, errors.New("could not determine max_user_watchers")
	}
	if errCat != nil { return 0, errCat }
	if errParseUint != nil { return 0, errParseUint }
	if maxNrWatchers == 0 { return 0, errors.New("could not determine max_user_watchers") }

	return maxNrWatchers, nil
}
func (watcher *InotifyWatcher) getRequiredNrWatchers(nrFiles uint64) uint64 {
	nrWatchers := uint64(1)
	for nrFiles > nrWatchers / 2 {
		nrWatchers *= 2
	}
	return nrWatchers
}
func (watcher *InotifyWatcher) setMaxUserWatchers(nrWatchers uint64) error {
	args := []string{
		"sysctl",
		"fs.inotify.max_user_watches=" + strconv.FormatUint(nrWatchers, 10),
	}
	fmt.Println("Higher number of max_user_watchers required")
	fmt.Println("sudo " + strings.Join(args, " "))
	command := exec.Command("sudo", args...)
	command.Stdin = os.Stdin

	return command.Run()
}
