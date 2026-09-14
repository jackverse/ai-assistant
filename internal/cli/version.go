package cli

import (
	"fmt"
	"io"

	"winclean/internal/version"
)

func cmdVersion(w io.Writer) int {
	fmt.Fprintln(w, version.String())
	return ExitOK
}
