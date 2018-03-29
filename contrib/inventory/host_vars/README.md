# host variables

To assign variables only for some specific groups use this directory.
Create a file here with the node name and then put the vars inside.

Example: create the file "node1" (is the node name from hosts file)
```
GCE: true
```

This place is the most useful when using different Windows nodes with
different passwords. For example 2 Windows Server nodes can have the
same username and different passwords.

Just create a file for the nodes that have different password from the
one present in group_vars. Example for "node5":
```
ansible_user: Administrator
ansible_password: different_password
```