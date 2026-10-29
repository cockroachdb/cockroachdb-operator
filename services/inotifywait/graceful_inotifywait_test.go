package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildInotifyWaitCommand(t *testing.T) {
	type testCase struct {
		processName   string
		fileNames     []string
		outputCommand string
	}

	testCases := []testCase{
		{
			"cockroach",
			[]string{"cockroach/cockroach-certs"},
			"\n\nwhile inotifywait -e close_write,moved_to,create cockroach/cockroach-certs;\ndo\n\tpkill -HUP cockroach;\ndone\n",
		},
		{
			"prometheus",
			[]string{"/etc/prometheus", "/etc/prometheus/rules"},
			"\n\nwhile inotifywait -e close_write,moved_to,create /etc/prometheus /etc/prometheus/rules;\ndo\n\tpkill -HUP prometheus;\ndone\n",
		},
	}

	for _, tc := range testCases {
		cmd := buildInotifyWaitCommand(tc.processName, tc.fileNames)
		require.Equal(t, tc.outputCommand, cmd)
	}
}
