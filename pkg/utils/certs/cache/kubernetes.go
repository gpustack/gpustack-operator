package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	klog "k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/utils/certs"
	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

const (
	k8sManagedLabel      = "certs.gpustack.ai/managed"
	k8sManagedGroupLabel = "certs.gpustack.ai/group"

	k8sManagedNameSumAnno  = "certs.gpustack.ai/name-sum"
	k8sManagedNameAnno     = "certs.gpustack.ai/name"
	k8sManagedValueSumAnno = "certs.gpustack.ai/value-sum"
	k8sManagedValueKey     = "value"
)

// k8sCache implements certs.Cache using the Kubernetes Secret to store the certificate data.
type k8sCache struct {
	logger klog.Logger
	cli    certs.SecretInterface
	inf    cache.SharedIndexInformer
	grp    string
}

// NewK8sCache creates a new k8sCache instance with the given client.
func NewK8sCache(ctx context.Context, group string, cli certs.SecretInterface) (certs.Cache, error) {
	lg := klog.Background().WithName("certs").WithName("k8s")

	lw := func() *cache.ListWatch {
		labelSelector := labels.FormatLabels(map[string]string{
			k8sManagedLabel:      "true",
			k8sManagedGroupLabel: group,
		})

		return &cache.ListWatch{
			ListWithContextFunc: func(ctx context.Context, options meta.ListOptions) (runtime.Object, error) {
				options.ResourceVersion = "0"
				options.LabelSelector = labelSelector
				return cli.List(ctx, options)
			},
			WatchFuncWithContext: func(ctx context.Context, options meta.ListOptions) (watch.Interface, error) {
				options.LabelSelector = labelSelector
				return cli.Watch(ctx, options)
			},
		}
	}()

	inf := cache.NewSharedIndexInformer(lw, &core.Secret{}, 1*time.Hour,
		map[string]cache.IndexFunc{
			"_": func(obj any) ([]string, error) {
				s, ok := obj.(*core.Secret)
				if !ok {
					return nil, errors.New("object is not a secret")
				}

				if s.DeletionTimestamp != nil ||
					s.Type != core.SecretTypeOpaque ||
					s.Annotations == nil || s.Data == nil {
					return nil, nil
				}

				annos, data := s.Annotations, s.Data

				if annos[k8sManagedNameAnno] == "" || annos[k8sManagedNameSumAnno] == "" ||
					data[k8sManagedValueKey] == nil || annos[k8sManagedValueSumAnno] == "" {
					return nil, nil
				}

				if sumName(annos[k8sManagedNameAnno]) != annos[k8sManagedNameSumAnno] ||
					sumValue(data[k8sManagedValueKey]) != annos[k8sManagedValueSumAnno] {
					lg.Error(nil, "invalid key %q", annos[k8sManagedNameAnno])
					return nil, nil
				}

				// Index the secret holding the key only, so that a key resolves to one secret
				// at most. Anything else claiming the key, like a secret an older release
				// created under a generated name, is left alone.
				if s.Name != secretName(group, annos[k8sManagedNameAnno]) {
					return nil, nil
				}

				return []string{annos[k8sManagedNameAnno]}, nil
			},
		})

	gox.Go(func() {
		inf.Run(ctx.Done())
	})

	// Wait for the informer to sync.
	{
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		if !cache.WaitForCacheSync(ctx.Done(), inf.HasSynced) {
			return k8sCache{}, errors.New("sync informer")
		}
	}

	return k8sCache{
		logger: lg,
		cli:    cli,
		inf:    inf,
		grp:    group,
	}, nil
}

// Get reads a certificate data from the specified secret name.
func (k k8sCache) Get(_ context.Context, name string) ([]byte, error) {
	if name == "" {
		return nil, certs.ErrCacheMiss
	}

	// Get existed secret.
	sec := k.get(name)
	if sec == nil || sec.DeletionTimestamp != nil {
		return nil, certs.ErrCacheMiss
	}

	return sec.Data[k8sManagedValueKey], nil
}

// Put writes the certificate data to specified secret name.
//
// The secret holding a key is named after that key, so writers racing on one key create or
// update one secret, whichever of them gets there first.
func (k k8sCache) Put(ctx context.Context, name string, data []byte) error {
	if name == "" || len(data) == 0 {
		return nil
	}

	sec := &core.Secret{
		ObjectMeta: meta.ObjectMeta{
			Name: secretName(k.grp, name),
			Annotations: map[string]string{
				k8sManagedNameAnno:     name,
				k8sManagedNameSumAnno:  sumName(name),
				k8sManagedValueSumAnno: sumValue(data),
			},
			Labels: map[string]string{
				k8sManagedLabel:      "true",
				k8sManagedGroupLabel: k.grp,
			},
		},
		Type: core.SecretTypeOpaque,
		Data: map[string][]byte{
			k8sManagedValueKey: data,
		},
	}

	secAlignFn := func(aSec *core.Secret) (_ *core.Secret, skip bool, err error) {
		skip = true
		// Align annotations.
		for ak, av := range sec.Annotations {
			if aSec.Annotations[ak] == av {
				continue
			}
			if aSec.Annotations == nil {
				aSec.Annotations = make(map[string]string, len(sec.Annotations))
			}
			aSec.Annotations[ak] = av
			skip = false
		}
		// Align labels.
		for lk, lv := range sec.Labels {
			if aSec.Labels[lk] == lv {
				continue
			}
			if aSec.Labels == nil {
				aSec.Labels = make(map[string]string, len(sec.Labels))
			}
			aSec.Labels[lk] = lv
			skip = false
		}
		// Align value.
		if !bytes.Equal(aSec.Data[k8sManagedValueKey], data) {
			if aSec.Data == nil {
				aSec.Data = make(map[string][]byte, 1)
			}
			aSec.Data[k8sManagedValueKey] = data
			skip = false
		}
		return aSec, skip, err
	}

	asec, err := kubeclientset.Update(ctx, k.cli, sec,
		kubeclientset.WithCreateIfNotExisted[*core.Secret](),
		kubeclientset.WithUpdateAlign(secAlignFn))
	if err != nil {
		return fmt.Errorf("put secret %q: %w", sec.Name, err)
	}

	k.logger.V(4).Info("put secret", "object", klog.KObj(asec))

	return nil
}

// Delete removes the specified secret name.
func (k k8sCache) Delete(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}

	// Delete existed secret.
	sn := secretName(k.grp, name)
	err := k.cli.Delete(ctx, sn,
		meta.DeleteOptions{
			PropagationPolicy: ptr.To(meta.DeletePropagationBackground),
		})
	if err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("delete secret %q: %w", sn, err)
	}

	k.logger.V(4).Info("deleted secret", "name", sn)

	return nil
}

func (k k8sCache) get(name string) *core.Secret {
	if name == "" {
		return nil
	}

	secs, err := k.inf.GetIndexer().ByIndex("_", name)
	if err != nil {
		k.logger.Error(err, "get indexed cached secrets")
		return nil
	}

	// Only the secret named after the key is indexed, so there is at most one.
	if len(secs) == 0 {
		return nil
	}

	return secs[0].(*core.Secret)
}

// secretName returns the name of the secret holding the given key of the given group.
//
// The name is a digest, as a key is not constrained to what an object name allows.
func secretName(group, key string) string {
	return "gpustack-cert-" + stringx.SumByFNV64a(group, "/", key)
}

func sumName(k string) string {
	return "fnv64a:" + stringx.SumByFNV64a(k)
}

func sumValue(v []byte) string {
	return "sha224:" + stringx.SumBytesBySHA224(v)
}
