#!/bin/bash

workdir=$(cd ../ && pwd)
substitute_string='pkg/testing/mocks'
replacement_string='vendor'

# verify that go is installed
gocmd_result=$(go env | grep GOPATH | tr -d '"')
if [ $? -eq 0 ]
then
   echo $cmd_result
else
   echo "ERROR: please install go"
   exit 1
fi

# check if mockery is installed
mockery_cmd_res=$(mockery --version)
VER=2.2.1
if [ $? != 0 ]
then
  GOBINPATH=${gocmd_result//GOPATH=/}/bin
  echo $GOBINPATH
  echo "mockery not installed, installing mockery ...."
  wget https://github.com/vektra/mockery/releases/download/v${VER}/mockery_${VER}_Linux_x86_64.tar.gz -P $GOBINPATH
  tar -xf $GOBINPATH/mockery_${VER}_Linux_x86_64.tar.gz -C $GOBINPATH
  mockery --version
else
  echo "mockery is already installed"
fi

for mockfilename in $(find  ../pkg/testing/mocks -name '*.go')
do
  echo $mockfilename
  # STEP 1: get the interface name to be mocked by first getting the file name, followed by stripping off the `.go` extension
  # STEP 1.1: split the string based on the delimiter '/' and  retrieve the last occurence to get the file name with the '.go' extension
  interface_file_name_path=${mockfilename##*/}
  # STEP 1.2: split the string based on the delimiter '.go' and  retrieve the first occurence to obtain the interface name
  interface_name=${interface_file_name_path%%.go*}

  # Step 2: get the destination directory path where mock has to be re-generated at as well as the vendory directory path where the interface is defined
  # Step 2.1: get the destination directory path by truncating the file name
  mock_dest_dir=${mockfilename%$interface_file_name_path}
  # Step 2.1.1: remove the leading '../' from the path
  mock_dest_dir=${mock_dest_dir//..\//}

  # Step 2.2: obtain the vendor source directory path where the interface is defined that needs to be mocked/
  # this is obtained by replacing 'pkg/testing/mocks' in mock_dest_dir with 'vendor'
  mock_src_dir=${mock_dest_dir//$substitute_string/$replacement_string}

  # Step 3: regenerate the mock for the interface
  #docker run -v $workdir -w /src vektra/mockery --name $interface_name --dir $mock_src_dir --output $mock_dest_dir
  mockery -name $interface_name --dir $workdir/$mock_src_dir --output $workdir/$mock_dest_dir

done
