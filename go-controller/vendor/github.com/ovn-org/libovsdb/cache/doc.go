/*
Package cache provides a cache of model.Model elements that can be used in an OVSDB client or server.

The cache can be accessed using a simple API:

    cache.Table("Open_vSwitch").Row("<ovs-uuid>")

It implements the ovsdb.NotificationHandler interface
such that it can be populated automatically by
update notifications

It also contains an eventProcessor where callers
may registers functions that will get called on
every Add/Update/Delete event.
*/
package cache
