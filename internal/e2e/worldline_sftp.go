package e2e

import (
	"fmt"

	"github.com/webduvet/fintechlab-simple/internal/wlsftp"
)

// downloadWorldlineFile dials the acquirer's real SFTP channel and
// downloads one file from its download/ directory, as it lies there:
// Worldline leaves its settlement files unencrypted.
//
// This is the e2e suite's own second opinion. The platform pulls the same
// file through the same channel; having the e2e suite do it independently is
// what makes a passing run mean the bytes were really on the server,
// rather than that the platform reported success about a file only it
// could see.
func downloadWorldlineFile(env *Env, filename string) ([]byte, error) {
	c, err := wlsftp.Dial(wlsftp.ClientConfig{
		Host:     env.WorldlineSFTPHost,
		Port:     env.WorldlineSFTPPort,
		User:     "harness",
		Password: env.WorldlineSFTPPassword,
		// No host-key pinning: this is a throwaway test client, and the
		// platform side (cmd/settlement) is where pinning is configured
		// and therefore where it matters that it works.
	})
	if err != nil {
		return nil, fmt.Errorf("e2e: %w", err)
	}
	defer c.Close()

	body, err := c.Download(filename, nil)
	if err != nil {
		return nil, fmt.Errorf("e2e: %w", err)
	}
	return body, nil
}
