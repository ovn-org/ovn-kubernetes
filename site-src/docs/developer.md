# Developer Documentation

This file aims to have information that is useful to the people contributing to this repo.

## Generating ovsdb bindings using modelgen

In order to generate the latest NBDB and SBDB bindings, we have a tool called `modelgen`
which lives in the libovsdb repo: https://github.com/ovn-org/libovsdb#modelgen. It is a
[code generator](https://go.dev/blog/generate) that uses `pkg/nbdb/gen.go` and `pkg/sbdb/gen.go`
files to auto-generate the models and additional code like deep-copy methods.

In order to use this tool do the following:
```
$ cd go-controller/
$ make modelgen
curl -sSL https://raw.githubusercontent.com/ovn-org/ovn/${OVN_SCHEMA_VERSION}/ovn-nb.ovsschema -o pkg/nbdb/ovn-nb.ovsschema
curl -sSL https://raw.githubusercontent.com/ovn-org/ovn/${OVN_SCHEMA_VERSION}/ovn-sb.ovsschema -o pkg/sbdb/ovn-sb.ovsschema
hack/update-modelgen.sh
```

If there are new bindings then you should see the changes being generated in the `pkg/nbdb` and
`pkg/sbdb` parts of the repo. Include them and push a commit!

NOTE1: You have to pay attention to the version of the commit hash used to download the modelgen
client. While the client doesn't change too often it can also become outdated causing wrong
generations. So keep in mind to re-install modelgen with latest commits and change the hash
value in the `hack/update-modelgen.sh` file if you find it outdated.

NOTE2: From time to time we always bump our fedora version of OVN used by KIND. But we oftentimes
forget to update the `OVN_SCHEMA_VERSION` in our `Makefile` which is used to download the ovsdb schema.
If that version seems to be outdated, probably best to update that as well and re-generate the schema
bindings.

## Generating CRD yamls using codegen

In order to generate the latest yaml files for a given CRD or to add a new CRD, once
the `types.go` has been created according to sig-apimachinery docs, the developer can run
`make codegen` to be able to generate all the clientgen, listers and informers for the new
CRD along with the deep-copy methods and actual yaml files which get created in `_output/crd`
folder and are copied over to `dist/templates` to then be used when creating a KIND cluster.
