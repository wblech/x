package ansi

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi/parser"
)

func TestDcsSequence(t *testing.T) {
	cases := []testCase{
		{
			name:  "max_params",
			input: fmt.Sprintf("\x1bP%sp\x1b\\", strings.Repeat("1;", 33)),
			expected: []any{
				dcsSequence{
					Cmd:    'p',
					Params: Params{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1},
					Data:   []byte{},
				},
				Cmd('\\'),
			},
		},
		{
			name:  "reset",
			input: "\x1b[3;1\x1bP1$tx\x9c",
			expected: []any{
				dcsSequence{
					Cmd:    't' | '$'<<parser.IntermedShift,
					Params: Params{1},
					Data:   []byte{'x'},
				},
			},
		},
		{
			name:  "parse",
			input: "\x1bP0;1|17/ab\x9c",
			expected: []any{
				dcsSequence{
					Cmd:    '|',
					Params: Params{0, 1},
					Data:   []byte("17/ab"),
				},
			},
		},
		{
			name:  "intermediate_reset_on_exit",
			input: "\x1bP=1sZZZ\x1b+\x5c",
			expected: []any{
				dcsSequence{
					Cmd:    's' | '='<<parser.PrefixShift,
					Params: Params{1},
					Data:   []byte("ZZZ"),
				},
				Cmd(0x5c | '+'<<parser.IntermedShift),
			},
		},
		{
			name:  "put_utf8",
			input: "\x1bP+r😃\x1b\\",
			expected: []any{
				dcsSequence{
					Cmd:    'r' | '+'<<parser.IntermedShift,
					Params: Params{},
					Data:   []byte("😃"),
				},
				Cmd('\\'),
			},
		},
		{
			// DCS payload carries a UTF-8 Dingbat (E2 9C B3 = U+2733 ✳).
			// The 0x9C inside it must NOT terminate the passthrough; ESC\\ does.
			// Regression for watchtower Problem 5 class of bug in DCS.
			name:  "dcs_payload_with_dingbat_utf8",
			input: "\x1bPq\xe2\x9c\xb3 data\x1b\\",
			expected: []any{
				dcsSequence{
					Cmd:    'q',
					Params: Params{},
					Data:   []byte("\xe2\x9c\xb3 data"),
				},
				Cmd('\\'),
			},
		},
		{
			// Bare 0x9C (ST in C1) still terminates DCS when not in a UTF-8 sequence.
			// Regression for the 7-bit / non-UTF-8 path.
			name:  "dcs_bare_9c_terminates",
			input: "\x1bPqhello\x9cZ",
			expected: []any{
				dcsSequence{
					Cmd:    'q',
					Params: Params{},
					Data:   []byte("hello"),
				},
				'Z',
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dispatcher := &testDispatcher{}
			parser := testParser(dispatcher)
			parser.Parse([]byte(c.input))
			assertEqual(t, len(c.expected), len(dispatcher.dispatched))
			assertEqual(t, c.expected, dispatcher.dispatched)
		})
	}
}
