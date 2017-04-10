#!/bin/bash


# os::build::binaries_from_targets take a list of build targets and return the
# full go package to be built
function os::build::binaries_from_targets() {
  local target
  for target; do
    echo "${OS_GO_PACKAGE}/${target}"
  done
}
readonly -f os::build::binaries_from_targets

# os::util::absolute_path returns the absolute path to the directory provided
function os::util::absolute_path() {
        local relative_path="$1"
        local absolute_path

        pushd "${relative_path}" >/dev/null
        relative_path="$( pwd )"
        if [[ -h "${relative_path}" ]]; then
                absolute_path="$( readlink "${relative_path}" )"
        else
                absolute_path="${relative_path}"
        fi
        popd >/dev/null

        echo "${absolute_path}"
}
readonly -f os::util::absolute_path


