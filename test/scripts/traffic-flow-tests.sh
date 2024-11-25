#!/usr/bin/env bash

set -euo pipefail

# Set default values
export KUBECONFIG="${KUBECONFIG:-${HOME}/ovn.conf}"
export OCI_BIN="${KIND_EXPERIMENTAL_PROVIDER:-docker}"
export TFT_TEST_IMAGE="ghcr.io/wizhaoredhat/ocp-traffic-flow-tests:latest"
export TRAFFIC_FLOW_TESTS_DIRNAME="ocp-traffic-flow-tests"
export TRAFFIC_FLOW_TESTS_REPO="https://github.com/wizhaoredhat/ocp-traffic-flow-tests.git"
export TRAFFIC_FLOW_TESTS_COMMIT="eb46da7a3ee1c3cfab5e141f69a3ccd1cdbfbabd"
export TRAFFIC_FLOW_TESTS="${TRAFFIC_FLOW_TESTS:-1,2,3}"

export SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &> /dev/null && pwd)"
export TOP_DIR="/mnt/runner"
export TRAFFIC_FLOW_TESTS_FULL_PATH="${TOP_DIR}/${TRAFFIC_FLOW_TESTS_DIRNAME}"

log() {
    local level="$1"
    shift
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [$level] $*"
}

error_exit() {
    log "ERROR" "$*"
    exit 1
}

check_dependencies() {
    local dependencies=(sed pip git kubectl kind "${OCI_BIN}")
    for cmd in "${dependencies[@]}"; do
        if ! command -v "${cmd}" &> /dev/null; then
            error_exit "Dependency not met: ${cmd}"
        fi
    done
}

usage() {
    echo "Usage: $(basename "$0") {setup|run}"
}

create_config_file_defaults() {
    local file_name="${1:-config.yaml}"

    cat <<EOT > "$file_name"
tft:
  - name: "Github Workflow Test"
    namespace: "default"
    test_cases: "TBD"
    duration: "10"
    connections:
      - name: "Connection_1"
        type: "iperf-tcp"
        instances: 1
        server:
          - name: "ovn-worker"
            persistent: "false"
            sriov: "false"
        client:
          - name: "ovn-worker2"
            sriov: "false"
        plugins: []
kubeconfig: "TBD"
kubeconfig_infra:
EOT
}

process_test_results() {
    local result_file="$1"

    log "INFO" "Processing test results from: ${result_file}"
    ./print_results.py ${result_file} || error_exit "Results evaluation failed."
}

setup() {
    check_dependencies

    "${OCI_BIN}" pull "${TFT_TEST_IMAGE}" || error_exit "Failed to pull the test image."
    local KIND_CLUSTER_NAME
    KIND_CLUSTER_NAME=$(kind get clusters)
    kind load docker-image "${TFT_TEST_IMAGE}" --name "${KIND_CLUSTER_NAME}" || error_exit "Failed to load the test image."

    if [ -d "${TRAFFIC_FLOW_TESTS_FULL_PATH}" ]; then
        error_exit "Install folder already exists: ${TRAFFIC_FLOW_TESTS_FULL_PATH}"
    fi
    mkdir -pv "${TRAFFIC_FLOW_TESTS_FULL_PATH}"
    cd "${TRAFFIC_FLOW_TESTS_FULL_PATH}"

    git init
    git remote add origin "${TRAFFIC_FLOW_TESTS_REPO}"
    git fetch --depth=1 origin "${TRAFFIC_FLOW_TESTS_COMMIT}"
    git checkout "${TRAFFIC_FLOW_TESTS_COMMIT}"

    python -m venv tft-venv
    source tft-venv/bin/activate
    pip install --upgrade pip
    pip install -r requirements.txt

    local CONFIG_FILE="config.yaml"
    create_config_file_defaults "$CONFIG_FILE"

    sed -i "s|^kubeconfig:.*|kubeconfig: \"$KUBECONFIG\"|" "$CONFIG_FILE"
    sed -i "s|test_cases:.*|test_cases: \"$TRAFFIC_FLOW_TESTS\"|" "$CONFIG_FILE"

    log "INFO" "Setup complete. Configuration file created: $CONFIG_FILE"

    echo
    echo "---"
    cat "$CONFIG_FILE"
}

run() {
    cd "${TRAFFIC_FLOW_TESTS_FULL_PATH}" || error_exit "Run folder not found. Missing setup?"
    source tft-venv/bin/activate || error_exit "Python environment missing. Missing setup?"

    local OUTPUT_BASE="${TRAFFIC_FLOW_TESTS_FULL_PATH}/ft-logs/result-"
    time ./tft.py config.yaml -o "$OUTPUT_BASE" || error_exit "Test execution FAILED."
    local RESULT_FILE=$(ls -rt "${OUTPUT_BASE}"*.json | tail -1)
    cp -vf "${RESULT_FILE}" /tmp/traffic_flow_test_result.json  || error_exit "Unable to locate results.json."

    log "INFO" "Results saved to /tmp/traffic_flow_test_result.json"

    process_test_results "${RESULT_FILE}"
}

case "${1:-}" in
    setup)
        setup
        ;;
    run)
        run
        ;;
    -h|--help)
        usage
        ;;
    *)
        usage
        exit 1
        ;;
esac
