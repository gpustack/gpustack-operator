package store

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCacheClaimProcess(t *testing.T) {
	mode := os.Getenv("GPUSTACK_TEST_CACHE_CLAIM_MODE")
	if mode == "" {
		return
	}
	s, err := Open(os.Getenv("GPUSTACK_TEST_CACHE_CLAIM_ROOT"))
	require.NoError(t, err)
	fmt.Println("ready")
	var changed bool
	switch mode {
	case "claim":
		changed, err = s.Claim(t.Context(), Ref{VolumeID: "child", Hex: testHex})
	case "rebuild":
		_, err = s.Rebuild(t.Context(), nil, "9:9")
	case "claim-deadline":
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		defer cancel()
		_, err = s.Claim(ctx, Ref{VolumeID: "child", Hex: testHex})
		if errors.Is(err, context.DeadlineExceeded) {
			fmt.Println("deadline")
			return
		}
	case "claim-wait":
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		changed, err = s.Claim(ctx, Ref{VolumeID: "child", Hex: testHex})
	default:
		changed, err = s.RemoveUnreferenced(t.Context(), testHex)
	}
	require.NoError(t, err)
	fmt.Println(changed)
}

func TestCacheClaimCollectionAcrossProcesses(t *testing.T) {
	for _, mode := range []string{"claim", "collect", "rebuild"} {
		t.Run(mode, func(t *testing.T) {
			s := openTestStore(t)
			publishTestTree(t, s, testHex)
			unlock, err := s.lockClaims(context.Background())
			require.NoError(t, err)
			t.Cleanup(unlock)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCacheClaimProcess$")
			cmd.Env = append(os.Environ(), "GPUSTACK_TEST_CACHE_CLAIM_MODE="+mode, "GPUSTACK_TEST_CACHE_CLAIM_ROOT="+s.Root())
			stdout, err := cmd.StdoutPipe()
			require.NoError(t, err)
			cmd.Stderr = os.Stderr
			require.NoError(t, cmd.Start())
			t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
			scanner := bufio.NewScanner(stdout)
			require.True(t, scanner.Scan())
			require.Equal(t, "ready", scanner.Text())
			result := make(chan string, 1)
			go func() {
				if scanner.Scan() {
					result <- scanner.Text()
				} else {
					result <- "missing result"
				}
			}()
			select {
			case early := <-result:
				t.Fatalf("another process bypassed the cache claim lock: %s", early)
			case <-time.After(100 * time.Millisecond):
			}
			switch mode {
			case "claim":
				_, err = s.trashPublished(testHex)
			case "rebuild":
				err = s.WriteRef(Ref{VolumeID: "model-transfer.parent", Hex: testHex})
			default:
				err = s.WriteRef(Ref{VolumeID: "parent", Hex: testHex})
			}
			require.NoError(t, err)
			unlock()
			select {
			case actual := <-result:
				assert.Equal(t, "false", actual)
			case <-ctx.Done():
				t.Fatal("cache operation remained blocked after unlocking")
			}
			for scanner.Scan() {
			}
			require.NoError(t, scanner.Err())
			require.NoError(t, cmd.Wait())
			assert.Equal(t, mode != "claim", s.IsPublished(testHex))
			_, found, err := s.ReadRef("child")
			require.NoError(t, err)
			assert.False(t, found)
		})
	}
}

// startClaimChild runs one TestCacheClaimProcess child against this store's root and answers a
// line reader of its stdout.
func startClaimChild(t *testing.T, s *Store, mode string, budget time.Duration) (*exec.Cmd, *bufio.Scanner, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCacheClaimProcess$")
	cmd.Env = append(os.Environ(), "GPUSTACK_TEST_CACHE_CLAIM_MODE="+mode, "GPUSTACK_TEST_CACHE_CLAIM_ROOT="+s.Root())
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	return cmd, bufio.NewScanner(stdout), cancel
}

func TestCacheClaimWaitHonorsDeadlineAcrossProcesses(t *testing.T) {
	s := openTestStore(t)
	publishTestTree(t, s, testHex)
	unlock, err := s.lockClaims(context.Background())
	require.NoError(t, err)
	t.Cleanup(unlock)

	cmd, scanner, cancel := startClaimChild(t, s, "claim-deadline", 15*time.Second)
	defer cancel()
	require.True(t, scanner.Scan())
	require.Equal(t, "ready", scanner.Text())

	start := time.Now()
	require.True(t, scanner.Scan(), "the child stopped before reporting its deadline")
	assert.Equal(t, "deadline", scanner.Text())
	assert.Less(t, time.Since(start), 10*time.Second, "the child gave up promptly")
	require.NoError(t, scanner.Err())
	require.NoError(t, cmd.Wait())

	_, found, err := s.ReadRef("child")
	require.NoError(t, err)
	assert.False(t, found, "a claim that gave up records nothing")
}

func TestCacheClaimWaitSucceedsAfterHolderReleases(t *testing.T) {
	s := openTestStore(t)
	publishTestTree(t, s, testHex)
	unlock, err := s.lockClaims(context.Background())
	require.NoError(t, err)

	cmd, scanner, cancel := startClaimChild(t, s, "claim-wait", 20*time.Second)
	defer cancel()
	require.True(t, scanner.Scan())
	require.Equal(t, "ready", scanner.Text())

	time.Sleep(200 * time.Millisecond)
	unlock()
	require.True(t, scanner.Scan(), "the child never claimed after the holder released")
	assert.Equal(t, "true", scanner.Text())
	require.NoError(t, scanner.Err())
	require.NoError(t, cmd.Wait())

	_, found, err := s.ReadRef("child")
	require.NoError(t, err)
	assert.True(t, found, "a claim that succeeded records its reference")
}
