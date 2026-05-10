# Deploy via [ytt](https://carvel.dev/ytt/)

```bash

# Default deployment without cert-manager
$ ytt -f . | kubectl apply -f -

# Deploy with cert-manager enabled
$ ytt -f . -v certmanagerEnabled=true | kubectl apply -f -

# Deploy in other namespace (default is `gpustack-operator-system`)
$ ytt -f . -v namespaceName=other-namespace | kubectl apply -f -

```