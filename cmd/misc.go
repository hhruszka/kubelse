package cmd

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
)

var (
	logBuffer chan string = make(chan string, runtime.NumCPU())
	logWg     sync.WaitGroup
)

func log(msg string) {
	if !quiet {
		logBuffer <- msg
	}
}

func stoplog() {
	close(logBuffer)
	logWg.Wait()
}

func init() {
	logWg.Add(1)
	go func() {
		defer logWg.Done()
		for msg := range logBuffer {
			fmt.Fprint(os.Stderr, msg)
		}
	}()
}

func untangleOption(option string) []string {
	if len(option) > 0 {
		return strings.Split(option, ",")
	}
	return nil
}

func promptYN(prompt string) bool {
	var response string

	for {
		log(fmt.Sprintf(prompt))
		_, err := fmt.Scanf("%s\n", &response)

		if err != nil {
			log(fmt.Sprintln("Error reading input. Please try again."))
			continue
		}

		response = strings.ToUpper(strings.TrimSpace(response))

		if response == "Y" {
			return true
		} else if response == "N" {
			return false
		} else {
			log(fmt.Sprintln("Invalid input. Please enter 'Y' or 'N'."))
		}
	}
}
