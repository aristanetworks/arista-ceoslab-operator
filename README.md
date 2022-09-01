# Arista cEOS-lab Operator
The Arista Networks cEOS-lab operator manages containerized EOS instances in k8s clusters managed
by KNE. It is based on operator-sdk.

## Usage
Deploy the manifest:
```
kubectl apply -f config/kustomized/manifest.yaml
```
This should be equivalent to `-k config/bases`. However, unit tests are run against the manifest
so this is preferred.

## Development Notes

### Testing
The operator uses [kuttl](https://kuttl.dev/) for testing. To run the test suite, make sure images
of the operator, cEOS-lab, and the init container are tagged as named in `kuttl-test.yaml` in the
repository's root. Kuttl can be used either through its binary or `kubectl kuttl`.

As mentioned above, these tests use the manifest file in `config/kustomized` to stage the test
cluster (kuttl does not support kustomize). If relevant changes are made to the kustomization files
or Makefile, for instance, when changing the version/tag or image name, this manifest should
be updated by `make manifests`.

### Versioning
The git tag will match the project version defined in the Makefile and the image tag present in
the manifest.

## How it works
This project aims to follow the Kubernetes [Operator pattern](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)

It uses [Controllers](https://kubernetes.io/docs/concepts/architecture/controller/) 
which provides a reconcile function responsible for synchronizing resources untile the desired state is reached on the cluster 

## License

Copyright 2022 Arista Networks

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
