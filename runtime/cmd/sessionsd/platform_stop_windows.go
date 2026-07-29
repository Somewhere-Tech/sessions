//go:build windows

package main

import (
	"log"
	"os"

	"golang.org/x/sys/windows"
)

type supervisorStopSignal struct{}

func (supervisorStopSignal) Signal() {}
func (supervisorStopSignal) String() string {
	return "per-user supervisor handoff"
}

func watchPlatformStop(stop chan<- os.Signal) func() {
	name := os.Getenv(supervisorStopEventEnv)
	if name == "" {
		return func() {}
	}
	handle, err := windows.OpenEvent(windows.SYNCHRONIZE, false, windows.StringToUTF16Ptr(name))
	if err != nil {
		log.Printf("sessionsd: open supervisor stop event: %v", err)
		return func() {}
	}
	go func() {
		event, err := windows.WaitForSingleObject(handle, windows.INFINITE)
		if err != nil {
			log.Printf("sessionsd: wait for supervisor stop event: %v", err)
			return
		}
		if event == windows.WAIT_OBJECT_0 {
			stop <- supervisorStopSignal{}
		}
	}()
	return func() {
		_ = windows.CloseHandle(handle)
	}
}
