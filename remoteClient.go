package main

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strings"
	"time"
)

type RemoteClient interface {
	io.Closer
	LoggerAware
	Upload(filenames []string) error
	Delete(filenames []string) error
	Move(from string, to string) error
	Ready() *Locker
}

type sshClient struct { // TODO: rename
	RemoteClient
	io.Closer
	config      Config
	sshCmd      string
	controlPath string
	masterReady *Locker
	done        bool // MAYBE: masterConnectionProcess
	logger      Logger
}
func (sshClient) New(config Config) *sshClient {
	controlPathFile, err := ioutil.TempFile("", "sshmirror-")
	PanicIf(err)
	controlPath := controlPathFile.Name()
	Must(os.Remove(controlPath))

	sshCmd := fmt.Sprintf(
		"ssh -o ControlMaster=auto -o ControlPath=%s -o ConnectTimeout=%d -o ConnectionAttempts=1",
		controlPath,
		config.connTimeout,
	)
	if config.identityFile != "" { sshCmd += " -i " + config.identityFile }

	var waitingMaster Locker

	client := &sshClient{
		config:      config,
		sshCmd:      sshCmd,
		controlPath: controlPath,
		masterReady: &waitingMaster,
		logger:      NullLogger{},
	}

	client.masterReady.Lock()
	go client.keepMasterConnection()

	return client
}
func (client *sshClient) Close() error {
	client.done = true
	client.closeMaster()
	_ = os.Remove(client.controlPath)
	return nil
}
func (client *sshClient) Upload(filenames []string) error {
	if client.runCommand(
		fmt.Sprintf(
			"rsync -azER -e '%s' -- %s %s:%s",
			client.sshCmd,
			strings.Join(escapeFilenames(filenames), " "),
			client.config.remoteHost,
			client.config.remoteDir,
		),
		nil,
	) {
		return nil
	} else {
		return errors.New("could not upload") // MAYBE: actual error
	}
}
func (client *sshClient) Delete(filenames []string) error {
	if client.runRemoteCommand(fmt.Sprintf(
		"rm -rf -- %s", // MAYBE: something more reliable
		strings.Join(escapeFilenames(filenames), " "),
	)) {
		return nil
	} else {
		return errors.New("cound not delete") // MAYBE: actual error
	}
}
func (client *sshClient) Move(from string, to string) error {
	if client.runRemoteCommand(fmt.Sprintf(
		"mv -- %s %s",
		wrapApostrophe(from),
		wrapApostrophe(to),
	)) {
		return nil
	} else {
		return errors.New("could not move") // MAYBE: actual error
	}
}
func (client *sshClient) Ready() *Locker {
	return client.masterReady
}
func (client *sshClient) SetLogger(logger Logger) {
	client.logger = logger
}
func (client *sshClient) keepMasterConnection() {
	client.closeMaster()

	for {
		fmt.Print("Establishing SSH Master connection... ") // MAYBE: stopwatch

		// MAYBE: check if it doesn't hang on server after disconnection
		client.runCommand(
			fmt.Sprintf(
				"%s -o ServerAliveInterval=%d -o ServerAliveCountMax=1 -M %s 'echo done && sleep infinity'",
				client.sshCmd,
				client.config.connTimeout,
				client.config.remoteHost,
			),
			func(out string) {
				fmt.Println(out)
				client.logger.Debug("master ready")
				client.masterReady.Unlock() // MAYBE: ensure this happens only once
			},
		)

		client.masterReady.Lock()
		client.closeMaster()
		if client.done { break }
		time.Sleep(time.Duration(client.config.connTimeout) * time.Second)
	}
}
func (client *sshClient) closeMaster() {
	client.runCommand(
		fmt.Sprintf("%s -O exit %s 2>/dev/null", client.sshCmd, client.config.remoteHost),
		nil,
	)
}
func (client *sshClient) runCommand(command string, onStdout func(string)) bool {
	client.logger.Debug("running command", command)

	return RunCommand(
		client.config.localDir,
		command,
		onStdout,
		client.logger.Error,
	)
}
func (client *sshClient) runRemoteCommand(command string) bool {
	return client.runCommand(
		fmt.Sprintf(
			"%s %s 'cd %s && (%s)'",
			client.sshCmd,
			client.config.remoteHost,
			client.config.remoteDir,
			escapeApostrophe(command),
		),
		nil,
	)
}

func escapeApostrophe(text string) string {
	//text = strings.Replace(text, "\\", "\\\\", -1)
	//text = strings.Replace(text, "'", "\\'", -1)
	//return text

	//return strings.Join(strings.Split(text, "'"), `'"'"'`)
	return strings.Replace(text, "'", `'"'"'`, -1)
}
func wrapApostrophe(text string) string {
	return fmt.Sprintf("'%s'", escapeApostrophe(text))
}
func escapeFilenames(filenames []string) []string {
	escapedFilenames := make([]string, 0, len(filenames))
	for _, filename := range filenames {
		escapedFilenames = append(escapedFilenames, wrapApostrophe(filename))
	}
	return escapedFilenames
}
