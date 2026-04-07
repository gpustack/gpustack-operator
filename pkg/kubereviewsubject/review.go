package kubereviewsubject

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	authz "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/util/cache"
	authnuser "k8s.io/apiserver/pkg/authentication/user"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	authzcli "k8s.io/client-go/kubernetes/typed/authorization/v1"
	klog "k8s.io/klog/v2"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/utils/stringx"
	"gpustack.ai/gpustack/pkg/utils/varx"
)

type (
	// Review holds the attributes for advanced reviewing.
	Review = authz.SubjectAccessReviewSpec
	// Reviews is the list of Review.
	Reviews = []Review
	// ResourceAttributes is the alias of authz.ResourceAttributes.
	ResourceAttributes = authz.ResourceAttributes
	// NonResourceAttributes is the alias of authz.NonResourceAttributes.
	NonResourceAttributes = authz.NonResourceAttributes
	// ExtraValue is the alias of authz.ExtraValue.
	ExtraValue = authz.ExtraValue

	// DeniedError is an error indicate which Review target has been denied.
	DeniedError struct {
		// Review holds the
		Review Review
	}
)

func (e DeniedError) Error() string {
	return fmt.Sprintf("denied %s", e.Review)
}

// IsDeniedError checks if the error is a DeniedError.
func IsDeniedError(err error) bool {
	return errors.As(err, &DeniedError{})
}

// Try ignores the DeniedError.
func Try(err error) error {
	if err != nil {
		if !IsDeniedError(err) {
			return err
		}
		klog.Error(err, "ignored subject review denied error, need fixing manually")
	}
	return nil
}

var (
	responseCache           = cache.NewLRUExpireCache(8192)
	responseAuthorizedTTL   = varx.NewOnce(10 * time.Second)
	responseUnauthorizedTTL = varx.NewOnce(10 * time.Second)
)

// ConfigureResponseTTL configures the TTL for authorized and unauthorized responses.
func ConfigureResponseTTL(authorized, unauthorized time.Duration) {
	responseAuthorizedTTL.Configure(authorized)
	responseUnauthorizedTTL.Configure(unauthorized)
}

// CanDo checks if the given subject can do all specified actions.
func CanDo(ctx context.Context, cli authzcli.SubjectAccessReviewInterface, reviews Reviews) error {
	if len(reviews) == 0 {
		return errors.New("no subject review to check")
	}

	for i := range reviews {
		sar := &authz.SubjectAccessReview{
			Spec: reviews[i],
		}

		var key string
		{
			bs, err := json.Marshal(sar.Spec)
			if err == nil {
				key = stringx.FromBytes(&bs)
			}
		}

		if entry, ok := responseCache.Get(key); ok {
			sar.Status = entry.(authz.SubjectAccessReviewStatus)
		} else {
			var err error
			sar, err = kubeclientset.Create(ctx, cli, sar)
			if err != nil {
				return fmt.Errorf("create subject access review %s: %w", reviews[i], err)
			}
		}

		if !sar.Status.Allowed {
			responseCache.Add(key, sar.Status, responseUnauthorizedTTL.Get())
			return DeniedError{Review: reviews[i]}
		} else {
			responseCache.Add(key, sar.Status, responseAuthorizedTTL.Get())
		}
	}

	return nil
}

// CanDoWithCtrlClient is similar to CanDo, but uses the ctrl client.
func CanDoWithCtrlClient(ctx context.Context, cli ctrlcli.Client, reviews Reviews) error {
	if len(reviews) == 0 {
		return errors.New("no subject review to check")
	}

	for i := range reviews {
		sar := &authz.SubjectAccessReview{
			Spec: reviews[i],
		}

		err := cli.Create(ctx, sar, &ctrlcli.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create subject access review %s: %w", reviews[i], err)
		}

		if !sar.Status.Allowed {
			return DeniedError{Review: reviews[i]}
		}
	}

	return nil
}

// CanRequestUserDo leverages CanDo to review the requesting subject can do all specified actions or not.
//
// CanRequestUserDo overrides all given reviews' user information
// after it successfully parse from the given context.
func CanRequestUserDo(ctx context.Context, cli authzcli.SubjectAccessReviewInterface, reviews Reviews) error {
	ui, ok := genericapirequest.UserFrom(ctx)
	if !ok {
		return errors.New("cannot retrieve kubernetes request user information from context")
	}

	return CanDo(ctx, cli, overrideUserInfo(reviews, ui))
}

// CanRequestUserDoWithCtrlClient is similar to CanRequestUserDo, but uses the ctrl client.
func CanRequestUserDoWithCtrlClient(ctx context.Context, cli ctrlcli.Client, reviews Reviews) error {
	ui, ok := genericapirequest.UserFrom(ctx)
	if !ok {
		return errors.New("cannot retrieve kubernetes request user information from context")
	}

	return CanDoWithCtrlClient(ctx, cli, overrideUserInfo(reviews, ui))
}

// CanSpecificUserDo leverages CanDo to review the specified subject can do all specified actions or not.
func CanSpecificUserDo(ctx context.Context, cli authzcli.SubjectAccessReviewInterface, reviews Reviews, user authnuser.Info) error {
	return CanDo(ctx, cli, overrideUserInfo(reviews, user))
}

// CanSpecificUserDoWithCtrlClient is similar to CanSpecificUserDo, but uses the ctrl client.
func CanSpecificUserDoWithCtrlClient(ctx context.Context, cli ctrlcli.Client, reviews Reviews, user authnuser.Info) error {
	return CanDoWithCtrlClient(ctx, cli, overrideUserInfo(reviews, user))
}

// overrideUserInfo overrides the given Reviews with the given user information.
func overrideUserInfo(reviews Reviews, ui authnuser.Info) Reviews {
	var (
		user  = ui.GetName()
		uid   = ui.GetUID()
		extra = func() (out map[string]authz.ExtraValue) {
			in := ui.GetExtra()
			if len(in) == 0 {
				return out
			}
			out = make(map[string]authz.ExtraValue, len(in))
			for i := range in {
				if len(in[i]) == 0 {
					continue
				}
				out[i] = slices.Clone(in[i])
			}
			return out
		}()
		groups = ui.GetGroups()
	)

	// Override user information.
	for i := range reviews {
		reviews[i].User = user
		reviews[i].UID = uid
		reviews[i].Extra = extra
		reviews[i].Groups = groups
	}

	return reviews
}
