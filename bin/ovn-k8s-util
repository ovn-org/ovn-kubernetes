#! /bin/sh

OVSLIB="/usr/share/openvswitch/scripts/ovs-lib"
if [ ! -e "${OVSLIB}" ]; then
    echo "$0 needs ${OVSLIB} to work." 1>&2
    exit 1
fi

if (ovs-vsctl --version) > /dev/null 2>&1; then :; else
    log_failure_msg "missing ovs-vsctl, cannot proceed."
    exit 1
fi

ovs_vsctl () {
    ovs-vsctl --timeout=5 "$@"
}

. ${OVSLIB} || exit 1

save_ip_address() {
    if [ -z "$1" ] || [ -z "$2" ]; then
        return
    fi
    port="$1"
    bridge="$2"

    echo "ip addr flush dev ${port} 2>/dev/null"
    ip addr show dev ${port} | while read addr; do
        set -- $addr

        # Check and trim family.
        family=$1
        shift
        case $family in
            inet | inet6) ;;
            *) continue ;;
        esac

        addrcmd=
        while test $# != 0; do
            case $1 in
                dynamic)
                    continue 2
                    ;;
                scope)
                    if test "$2" = link; then
                        continue 2
                    fi
                    ;;
                "${port}"|"${port}:"*)
                    shift
                    continue
                    ;;
            esac
            addrcmd="$addrcmd $1"
            shift
        done

        echo ip -f $family addr add $addrcmd dev ${bridge}
        echo ip link set ${bridge} up
    done
}

save_ip_route() {
    if [ -z "$1" ] || [ -z "$2" ]; then
        return
    fi
    port="$1"
    bridge="$2"

    echo "ip route flush dev ${port} proto boot 2>/dev/null" # Suppresses "Nothing to flush".
    ip route show dev ${port} | while read route; do
        # "proto kernel" routes are installed by the kernel automatically.
        case ${route} in
            *" proto kernel "*) continue ;;
        esac

        echo "ip route add ${route} dev ${bridge}"
    done
}

nics_to_bridge() {
    if (ip -V) > /dev/null 2>&1; then :; else
        log_failure_msg "missing ip command, cannot proceed."
        exit 1
    fi

    if (mktemp --version) > /dev/null 2>&1; then :; else
        log_failure_msg "missing mktemp command, cannot proceed."
        exit 1
    fi

    for iface in "$@"
    do
        if [ -d /sys/class/net/"${iface}" ]; then :; else
            log_failure_msg "interface ${iface} does not exist, skipping."
            continue
        fi

        if [ -e /sys/class/net/"${iface}/address" ]; then
            mac_address=`cat /sys/class/net/"${iface}"/address`
        else
            log_failure_msg "interface ${iface} does not have a mac address."
            continue
        fi

        OVSBR="br${iface}"
        if (ovs_vsctl add-br "${OVSBR}" \
                -- br-set-external-id "${OVSBR}" bridge-id "${OVSBR}"\
                -- set bridge "${OVSBR}" fail-mode=standalone \
                   other_config:hwaddr="$mac_address" \
                -- add-port "${OVSBR}" "${iface}") ; then
            log_success_msg "successfully created OVS bridge ${OVSBR}"

            #Move the ip address and ip route of the added port to the bridge.
            #To do this, save the current configuration of the NIC interface
            #as commands in a temp file and then re-execute it for the bridge.
            script=`mktemp`
            echo "#! /bin/sh" > ${script}

            save_ip_address "${iface}" "${OVSBR}" >> ${script}

            save_ip_route  "${iface}" "${OVSBR}" >> ${script}

            chmod +x ${script}
            if (${script}) then :; else
                log_failure_msg "failed to transfer IP address or routes from\
                                ${iface} to ${OVSBR}. Do it manually."
            fi
            rm -f ${script}
        else
            log_failure_msg "failed to create OVS bridge ${OVSBR}"
        fi
    done
}

usage() {
    UTIL=$(basename $0)
    cat << EOF
${UTIL}: Utilities for OVN integration in a host.
usage: ${UTIL} COMMAND

Commands:
  nics-to-bridge    creates an OVS bridge for each of the nic interfaces
                    provided after this command as arguments.
                    ex: ${UTIL} nics-to-bridge eth0 eth1 eth2
Options:
  -h, --help        display this help message.
EOF
    exit 0
}

case $1 in
    "nics-to-bridge")
        shift
        nics_to_bridge "$@"
        exit 0
        ;;
-h | --help)
        usage
        exit 0
        ;;
esac
