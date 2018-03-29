# Ansible inventory

Make sure to update the [hosts](/contrib/inventory/hosts) file with the nodes details.

Ensure that [group_vars](/contrib/inventory/group_vars) contain the right details to connect to the target servers.

For Linux nodes make sure that the username is set up correctly in variable ``ansible_ssh_user`` inside file [kube-master](/contrib/inventory/group_vars/kube-master).
Check the same for [kube-minions-linux](/contrib/inventory/group_vars/kube-minions-linux).

For Windows nodes, check the login credentials stored in [kube-minions-windows](/contrib/inventory/group_vars/kube-minions-windows). Make sure that ``ansible_user`` and ``ansible_password`` match to your target Windows servers.

Optional, in case you want to define different ssh users or login credentials for different minion nodes, please look into [host_vars](/contrib/inventory/host_vars).
