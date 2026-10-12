// onedriver-tray shows the state of running onedriver instances as a system
// tray icon (StatusNotifierItem) and offers a small control menu.
package main

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/godbus/dbus/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// runDetached starts a program without waiting for it.
func runDetached(name string, args ...string) {
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		log.Warn().Err(err).Str("cmd", name).Msg("Could not start program.")
		return
	}
	go cmd.Wait()
}

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		log.Fatal().Err(err).Msg("Could not connect to the session bus.")
	}
	t, err := NewTray(conn, Options{})
	if err != nil {
		log.Fatal().Err(err).Msg("Could not set up the tray icon.")
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigs; t.Quit() }()
	if err := t.Run(); err != nil {
		log.Fatal().Err(err).Msg("Tray failed.")
	}
}
