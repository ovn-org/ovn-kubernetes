#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

#
# Generate the LICENSES tree for Go module dependencies.

set -o errexit
set -o nounset
set -o pipefail

HERE=$(dirname "$(readlink --canonicalize "$BASH_SOURCE")")
GO_CONTROLLER_ROOT=$(readlink --canonicalize "${HERE}/..")
REPO_ROOT=$(readlink --canonicalize "${GO_CONTROLLER_ROOT}/..")
PROJECT_LICENSE_SOURCE=${PROJECT_LICENSE_SOURCE:-${REPO_ROOT}/LICENSE}
ROOT_LICENSES_README_SOURCE=${ROOT_LICENSES_README_SOURCE:-${REPO_ROOT}/LICENSES/README.md}

LICENSES_DIR=${LICENSES_DIR:-${REPO_ROOT}/LICENSES}
ROOT_LICENSE_OUTPUT=${ROOT_LICENSE_OUTPUT:-${REPO_ROOT}/LICENSE}
TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ovn-third-party-licenses.XXXXXX")
TMP_LICENSES_DIR=${TMP_DIR}/LICENSES
TMP_ROOT_LICENSE=${TMP_DIR}/LICENSE
TMP_DOWNLOADS_JSON=${TMP_DIR}/downloads.json
trap 'rm -rf "${TMP_DIR}"' EXIT

copy_file() {
    local source_file=$1
    local dest_file=$2

    mkdir -p "$(dirname "${dest_file}")"
    cp "${source_file}" "${dest_file}"
}

dir_has_readme_files() {
    local dir=$1

    find "${dir}" -type f \( -iname 'readme*' \) -print -quit | grep -q .
}

dir_has_non_readme_files() {
    local dir=$1

    find "${dir}" -type f ! \( -iname 'readme*' \) -print -quit | grep -q .
}

missing_license_files_allowed() {
    local module_path=$1
    local version=$2

    case "${module_path}@${version}" in
        # github.com/chzyer/logex@v1.1.10 ships no LICENSE/NOTICE/COPYING file,
        # and its downloaded README.md contains no license text to copy.
        github.com/chzyer/logex@v1.1.10)
            return 0
            ;;
        # github.com/lightstep/lightstep-tracer-common/golang/gogo at this commit
        # has no LICENSE file. README.md shows MIT license badge but contains no
        # license text. Declared as MIT-licensed in repository metadata.
        github.com/lightstep/lightstep-tracer-common/golang/gogo@v0.0.0-20190605223551-bc2310a04743)
            return 0
            ;;
    esac

    return 1
}

source_copy_override_paths() {
    local module_path=$1
    local version=$2
    case "${module_path}@${version}" in
        # These modules only expose licensing text through a README-like file.
        github.com/bmizerany/assert@v0.0.0-20160611221934-b7ed37b82869)
            printf '%s\n' "README.md"
            ;;
        github.com/garyburd/redigo@v0.0.0-20150301180006-535138d7bcd7)
            printf '%s\n' "README.markdown"
            ;;
        github.com/kr/logfmt@v0.0.0-20140226030751-b84e30acd515)
            printf '%s\n' "Readme"
            ;;
        github.com/JohnCGriffin/overflow@v0.0.0-20211019200055-46fa312c352c)
            printf '%s\n' "README.md"
            ;;
        github.com/hudl/fargo@v1.3.0)
            printf '%s\n' "README.md"
            ;;
        github.com/streadway/handy@v0.0.0-20190108123426-d5acb3125c2a)
            printf '%s\n' "README.md"
            ;;
        github.com/mattn/goveralls@v0.0.5)
            printf '%s\n' "README.md"
            ;;
    esac
}

has_source_copy_override() {
    local module_path=$1
    local version=$2

    source_copy_override_paths "${module_path}" "${version}" | grep -q .
}

copy_source_override() {
    local module_path=$1
    local version=$2
    local source_dir=$3
    local dest_dir=$4
    local rel_path
    local source_file
    local found=0

    while IFS= read -r rel_path || [[ -n "${rel_path}" ]]; do
        [[ -n "${rel_path}" ]] || continue

        source_file=${source_dir}/${rel_path}
        if [[ ! -f "${source_file}" ]]; then
            echo "source-copy override for ${module_path}@${version} references missing file ${rel_path}" >&2
            exit 1
        fi

        copy_file "${source_file}" "${dest_dir}/${rel_path}"
        found=1
    done < <(source_copy_override_paths "${module_path}" "${version}")

    if [[ ${found} -eq 0 ]]; then
        echo "source-copy override for ${module_path}@${version} does not define any files" >&2
        exit 1
    fi
}

is_ignored_legal_filename() {
    local lower_name=$1

    case "${lower_name}" in
        *.go|*.c|*.cc|*.cpp|*.cxx|*.h|*.hh|*.hpp|*.hxx|*.m|*.mm|*.rs|*.java|*.kt|*.kts|*.groovy|*.scala|*.py|*.rb|*.php|*.pl|*.pm|*.js|*.jsx|*.ts|*.tsx|*.sh|*.bash|*.zsh|*.fish|*.ps1|*.bat|*.cmd|*.yaml|*.yml|*.json|*.toml|*.xml|*.proto)
            return 0
            ;;
    esac

    return 1
}

copy_candidate_files() {
    local module_path=$1
    local version=$2
    local source_dir=$3
    local dest_dir=$4
    local found=0
    local candidate
    local lower_name
    local rel_path

    mkdir -p "${dest_dir}"

    while IFS= read -r candidate; do
        [[ -n "${candidate}" ]] || continue

        lower_name=$(basename "${candidate}" | tr '[:upper:]' '[:lower:]')
        if is_ignored_legal_filename "${lower_name}"; then
            continue
        fi
        rel_path=${candidate#"${source_dir}/"}
        mkdir -p "${dest_dir}/$(dirname "${rel_path}")"
        cp "${candidate}" "${dest_dir}/${rel_path}"
        found=1
    done < <(
        find "${source_dir}" -type f \
            \( -iname 'licen[sc]e*' -o -iname 'notice*' -o -iname 'copying*' -o -iname 'copyright*' \) \
            | LC_ALL=C sort
    )

    if [[ ${found} -ne 0 ]]; then
        if has_source_copy_override "${module_path}" "${version}"; then
            echo "source-copy override for ${module_path}@${version} is stale: upstream now ships license-like files" >&2
            echo "remove the override entry from ${HERE}/update-third-party-licenses.sh" >&2
            exit 1
        fi
        return
    fi

    if has_source_copy_override "${module_path}" "${version}"; then
        copy_source_override "${module_path}" "${version}" "${source_dir}" "${dest_dir}"
        if dir_has_readme_files "${dest_dir}" && dir_has_non_readme_files "${dest_dir}"; then
            echo "source-copy override for ${module_path}@${version} mixes README files with license files" >&2
            echo "keep the override README-only, or remove it if upstream now ships proper license files" >&2
            exit 1
        fi
        return
    fi

    if missing_license_files_allowed "${module_path}" "${version}"; then
        rm -rf "${dest_dir}"
        return
    fi

    echo "no license-like files found for ${module_path}@${version} in ${source_dir}" >&2
    echo "add an explicit exception to ${HERE}/update-third-party-licenses.sh" >&2
    exit 1
}

collect_module_downloads() {
    local module_dir
    local module_file
    local tmp_modfile
    local tmp_sumfile

    git -C "${REPO_ROOT}" ls-files -z -- \
        'go.mod' \
        ':(glob)**/go.mod' \
        | while IFS= read -r -d '' module_file; do
            module_dir=$(dirname "${REPO_ROOT}/${module_file}")
            (
                cd "${module_dir}"
                tmp_modfile=$(mktemp "${module_dir}/.third-party-licenses.XXXXXX.mod")
                tmp_sumfile=${tmp_modfile%.mod}.sum
                trap 'rm -f "${tmp_modfile}" "${tmp_sumfile}"' EXIT

                cp go.mod "${tmp_modfile}"
                if [[ -f go.sum ]]; then
                    cp go.sum "${tmp_sumfile}"
                fi

                GOWORK=off GOFLAGS=-mod=mod \
                    go mod download -modfile="$(basename "${tmp_modfile}")" -json all || true
            )
        done \
        | jq -s '.' > "${TMP_DOWNLOADS_JSON}"
}

generate_license_tree() {
    local package_path
    local resolved_version
    local resolved_dir
    local conflict=0
    local package_key
    declare -A package_dirs=()

    mkdir -p "${TMP_LICENSES_DIR}"
    cp "${PROJECT_LICENSE_SOURCE}" "${TMP_LICENSES_DIR}/LICENSE"
    cp "${PROJECT_LICENSE_SOURCE}" "${TMP_ROOT_LICENSE}"
    cp "${ROOT_LICENSES_README_SOURCE}" "${TMP_LICENSES_DIR}/README.md"

    if jq -e '.[] | select(.Error != null)' "${TMP_DOWNLOADS_JSON}" >/dev/null; then
        echo "ignoring unresolved module download entries without source directories:" >&2
        jq -r '.[] | select(.Error != null) | "  - \(.Path)@\(.Version): \(.Error)"' "${TMP_DOWNLOADS_JSON}" \
            | LC_ALL=C sort -u >&2
    fi

    while IFS=$'\t' read -r package_path resolved_version resolved_dir; do
            if [[ -z "${resolved_version}" ]]; then
                echo "missing version for ${package_path}" >&2
                exit 1
            fi

            if [[ ! -d "${resolved_dir}" ]]; then
                echo "module source directory does not exist: ${resolved_dir}" >&2
                exit 1
            fi

            package_key="${package_path}@${resolved_version}"
            if [[ -v "package_dirs[${package_key}]" ]]; then
                if [[ "${package_dirs[${package_key}]}" != "${resolved_dir}" ]]; then
                    echo "multiple source directories resolved for ${package_key}" >&2
                    conflict=1
                fi
                continue
            fi

            package_dirs["${package_key}"]="${resolved_dir}"
    done < <(
        jq -r --arg repo_root "${REPO_ROOT}" '
            .[]
            | select(.Main != true)
            | select(.Error == null)
            | {path: .Path, version: ((.Replace.Version // .Version) // ""), dir: ((.Replace.Dir // .Dir) // "")}
            | . as $resolved
            | select($resolved.dir != "")
            | select(($resolved.dir | startswith($repo_root)) | not)
            | [$resolved.path, $resolved.version, $resolved.dir]
            | @tsv
        ' "${TMP_DOWNLOADS_JSON}" \
            | LC_ALL=C sort -u
    )

    if [[ ${conflict} -ne 0 ]]; then
        exit 1
    fi

    if [[ ${#package_dirs[@]} -eq 0 ]]; then
        echo "no third-party module source directories were collected; refusing to overwrite ${LICENSES_DIR}" >&2
        exit 1
    fi

    for package_key in "${!package_dirs[@]}"; do
        package_path=${package_key%@*}
        resolved_version=${package_key##*@}
        copy_candidate_files \
            "${package_path}" \
            "${resolved_version}" \
            "${package_dirs[${package_key}]}" \
            "${TMP_LICENSES_DIR}/packages/${package_path}/${resolved_version}"
    done

}

publish_outputs() {
    local staged_dir="${LICENSES_DIR}.tmp.$$"
    local old_dir="${LICENSES_DIR}.old.$$"
    local staged_root_license="${ROOT_LICENSE_OUTPUT}.tmp.$$"
    local old_root_license="${ROOT_LICENSE_OUTPUT}.old.$$"

    mkdir -p "$(dirname "${LICENSES_DIR}")" "$(dirname "${ROOT_LICENSE_OUTPUT}")"
    rm -rf "${staged_dir}" "${old_dir}"
    rm -f "${staged_root_license}" "${old_root_license}"

    mv "${TMP_LICENSES_DIR}" "${staged_dir}"
    cp "${TMP_ROOT_LICENSE}" "${staged_root_license}"
    if [[ -e "${LICENSES_DIR}" ]]; then
        mv "${LICENSES_DIR}" "${old_dir}"
    fi
    if [[ -e "${ROOT_LICENSE_OUTPUT}" || -L "${ROOT_LICENSE_OUTPUT}" ]]; then
        mv "${ROOT_LICENSE_OUTPUT}" "${old_root_license}"
    fi
    mv "${staged_dir}" "${LICENSES_DIR}"
    mv "${staged_root_license}" "${ROOT_LICENSE_OUTPUT}"
    rm -rf "${old_dir}"
    rm -f "${old_root_license}"
}

collect_module_downloads
generate_license_tree
publish_outputs

echo "Updated ${LICENSES_DIR}"
