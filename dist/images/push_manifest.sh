#!/bin/bash

# Currently supported platforms of multi-arch images are: amd64 arm64
LINUX_ARCH=(amd64 arm64)
PLATFORMS=linux/${LINUX_ARCH[0]}
for i in $(seq 1  $[${#LINUX_ARCH[@]}-1])
do
    PLATFORMS=$PLATFORMS,linux/${LINUX_ARCH[$i]}
done

IMAGES_OVN=${1:-ovn-daemonset-ubuntu}
BRANCH_TAG=${2:-latest}
DOCKER_REPOSITORY=${3:-docker.io/ovnkube}
MANITOOL_VERSION=${4:-v1.0.0}

if [ `uname -m` = 'aarch64' ]
then
	BUILDARCH=arm64
elif [ `uname -m` = 'x86_64' ]
then
	BUILDARCH=amd64
fi


#Before push, 'docker login' is needed
push_multi_arch(){

       if [ ! -f "./manifest-tool" ]
       then
                sudo apt-get install -y jq
                wget https://github.com/estesp/manifest-tool/releases/download/${MANITOOL_VERSION}/manifest-tool-linux-${BUILDARCH} \
                -O manifest-tool && \
                chmod +x ./manifest-tool
       fi

       for IMAGE in "${IMAGES_OVN[@]}"
       do
         echo "multi arch image: ""${DOCKER_REPOSITORY}/${IMAGE}"
         ./manifest-tool push from-args --platforms ${PLATFORMS} --template ${DOCKER_REPOSITORY}/${IMAGE}-ARCH:${BRANCH_TAG} \
                --target ${DOCKER_REPOSITORY}/${IMAGE}:${BRANCH_TAG}
       done
}

echo "Push fat manifest for multi-arch ovnkube images:"
push_multi_arch

