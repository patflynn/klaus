package tmux_test

import (
	"os"
	"testing"

	"github.com/patflynn/klaus/internal/tmux/tmuxtest"
)

func TestMain(m *testing.M) { os.Exit(tmuxtest.Main(m)) }
