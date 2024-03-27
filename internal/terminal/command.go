package terminal

import (
	"io"

	"github.com/NHAS/reverse_ssh/internal/server/users"
)

type Command interface {
	// Returns the expected syntax for the command, used in the autocomplete process with text tokens to indicate where autocomplete can occur
	Expect(line ParsedLine) []string
	// Run the command with the given arguments
	Run(user *users.User, output io.ReadWriter, line ParsedLine) error
	// Give helptext for commands
	Help(explain bool) string
}
