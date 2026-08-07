package datax

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSnapshot(t *testing.T) {
	t.Run("zero value loads nil", func(t *testing.T) {
		var s Snapshot[int]
		assert.Nil(t, s.Load())
	})

	t.Run("latest value wins", func(t *testing.T) {
		var s Snapshot[int]
		for i := 0; i < 10; i++ {
			s.Store(&i)
		}
		got := s.Load()
		assert.NotNil(t, got)
		assert.Equal(t, 9, *got)
	})

	t.Run("concurrent store and load", func(t *testing.T) {
		var s Snapshot[int]

		const workers = 8
		const rounds = 1000

		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(2)
			go func(w int) {
				defer wg.Done()
				for r := 0; r < rounds; r++ {
					v := w*rounds + r
					s.Store(&v)
				}
			}(w)
			go func() {
				defer wg.Done()
				for r := 0; r < rounds; r++ {
					got := s.Load()
					if got != nil {
						assert.GreaterOrEqual(t, *got, 0)
						assert.Less(t, *got, workers*rounds)
					}
				}
			}()
		}
		wg.Wait()

		assert.NotNil(t, s.Load(), "a value must be stored after concurrent stores")
	})
}
