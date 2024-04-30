### How are the mock files for unit tests organized?
- The name of the mock file generated will be the same as the name of the `interface` definition.

- Mock files for `interfaces` defined in the `go-controller/vendor` directories are located in the 
`go-controller/pkg/testing/mocks` directory. The directory structure in the `go-controller/pkg/testing/mocks/` closely 
mimic the directory structure of  `go-controller/vendor/`.

	e.g; a) The `Cmd` interface defined in the `go-controller/vendor/k8s.io/utils/exec.go` file has its mock generated 
	in `go-controller/pkg/testing/mocks/k8s.io/utils/exec/Cmd.go` file
	
	e.g; b) The `Link` interface defined in the `go-controller/vendor/github.com/vishvananda/netlink/link.go` file has 
	its mock generated in `go-controller/pkg/testing/mocks/vishvananda/netlink/Link.go` file
	
- Mock files for `interfaces` defined in the non vendor directories of the project are located in the `mocks` directory 
of the same package as where the interface is defined.

	e.g; a) The `ExecRunner` interface defined in `go-controller/pkg/util/ovs.go` file has the its mock generated in 
	`go-controller/pkg/util/mocks/ExecRunner.go` file.
	
	e.g; b) The `SriovNetLibOps` interface defined in `go-controller/pkg/cni/helper_linux.go` file has its mock 
	generated in `go-controller/pkg/cni/mocks/SriovNetLibOps.go` file.
	
### How are the mocks for interfaces to be consumed by unit tests currently generated?

- The vektra/mockery package from https://github.com/vektra/mockery is leveraged to auto generate mocks for defined interfaces.

- Mocks for interfaces can be generated using vektra/mockery in one of two ways:
    
    - Using the binaries at https://github.com/vektra/mockery/releases
    
    - Using the docker image
    
- Sample commands to generate mocks when using the `binary` installed on a linux host.
    
    - Mock for interface `SriovNetLibOps` defined in the `go-controller/pkg/cni/helper_linux.go` file when executing the
    `mockery` command from dir `go-controller/`
    
        `mockery --name SriovnetLibOps --dir pkg/cni/ --output pkg/cni/mocks`
    
    - Mock for all interfaces defined in the vendor folder `go-controller/vendor/k8s.io/utils/exec` when executing the
    `mockery` command from dir `go-controller/`
    
        `mockery --all --dir vendor/k8s.io/utils/exec --output pkg/testing/mocks/k8s.io/utils/exec`
        
- Sample command to generate mocks when using the `docker` image

    - Mock for interface `SriovNetLibOps` defined in the `go-controller/pkg/cni/helper_linux.go` file when running the
    `docker` container from dir `go-controller/`
    
        `docker run -v $PWD:/src -w /src vektra/mockery --name SriovNetLibOps --dir pkg/cni/ --output pkg/cni/mocks`
        
    - Mock for all interfaces defined in the vendor folder `go-controller/vendor/k8s.io/utils/exec` when running the
    `docker` container from dir `go-controller/`
    
        `docker run -v $PWD:/src -w /src vektra/mockery --all --dir vendor/k8s.io/utils/exec --output pkg/testing/mocks/k8s.io/utils/exec`
    
### How to regenerate all existing mocks when interfaces (locally defined or in vendor libraries) are updated?

    - Execute the ```go-controller/hack/regenerate_vendor_mocks.sh``` in situations where all existing mocks have to be regenerated.
    NOTE: It would take a while(approx 20+ minutes) for all mocks to be regenerated.

### Reference links that explain how to use mocks with testify

- https://tutorialedge.net/golang/improving-your-tests-with-testify-go/ 

- https://techblog.fexcofts.com/2019/09/23/go-and-test-mocking/ 

- https://gowalker.org/github.com/stretchr/testify/mock 

- https://ncona.com/2020/02/using-testify-for-golang-tests/ 