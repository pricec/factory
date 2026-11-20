// Command vision asks the LAN vision model about an image and prints the
// text answer.
//
//	vision [--prompt "..."] <image path or http(s) URL>
//
// Env: FACTORY_VISION_URL (default http://192.168.1.147:11435),
// FACTORY_VISION_MODEL (default qwen2.5vl:7b). Exit status: 0 ok, 1 error,
// 2 usage, 3 endpoint unreachable. See docs/vision.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/pricec/factory/internal/vision"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("vision", flag.ContinueOnError)
	fs.SetOutput(stderr)
	prompt := fs.String("prompt", vision.DefaultPrompt, "question/instruction for the model")
	timeout := fs.Duration("timeout", vision.DefaultTimeout, "per-call timeout")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: vision [--prompt TEXT] [--timeout D] <image path or http(s) URL>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout+30*time.Second)
	defer cancel()
	img, err := vision.LoadImage(ctx, fs.Arg(0), nil)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	c := vision.FromEnv(getenv)
	c.Timeout = *timeout
	// Serialize with other vision calls on this host (shared GPU).
	unlock, err := vision.Lock(ctx, filepath.Join(os.TempDir(), "factory-vision.lock"))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer unlock()
	ans, err := c.Describe(ctx, img, *prompt)
	if err != nil {
		fmt.Fprintln(stderr, err)
		if errors.Is(err, vision.ErrUnreachable) {
			return 3
		}
		return 1
	}
	fmt.Fprintln(stdout, ans)
	return 0
}
