package ui

import (
	"os"
	"strings"

	"golang.org/x/term"
)

func init() {
	enableVirtualTerminal()
	if !colorsEnabled() {
		disableColors()
	}
}

func colorsEnabled() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

func disableColors() {
	ColorReset = ""
	ColorBold = ""
	ColorDim = ""
	ColorRed = ""
	ColorGreen = ""
	ColorYellow = ""
	ColorCyan = ""
}
