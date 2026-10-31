package broker

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestAuthorityReadErrorKeepsStructuralLossDistinct(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cause      error
		readFailed bool
	}{
		{name: "unreachable", cause: errors.New("API unavailable"), readFailed: true},
		{name: "server unavailable", cause: apierrors.NewServiceUnavailable("retry"), readFailed: true},
		{name: "missing", cause: apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "authority")},
		{name: "identity changed", cause: fmt.Errorf("%w: pin changed", ErrReservationUnavailable)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := reservationReadError("read authority", tc.cause)
			require.ErrorIs(t, err, ErrReservationUnavailable)
			require.ErrorIs(t, err, tc.cause)
			require.Equal(t, tc.readFailed, errors.Is(err, ErrReservationReadFailed))
		})
	}
}
