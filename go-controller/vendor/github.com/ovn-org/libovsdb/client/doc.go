/*
Package client connects to, monitors and interacts with OVSDB servers (RFC7047).

This package uses structs, that contain the 'ovs' field tag to determine which field goes to
which column in the database. We refer to pointers to this structs as Models. Example:

   type MyLogicalSwitch struct {
   	UUID   string            `ovsdb:"_uuid"` // _uuid tag is mandatory
   	Name   string            `ovsdb:"name"`
   	Ports  []string          `ovsdb:"ports"`
   	Config map[string]string `ovsdb:"other_config"`
   }

Based on these Models a Database Model (see ClientDBModel type) is built to represent
the entire OVSDB:

   clientDBModel, _ := client.NewClientDBModel("OVN_Northbound",
       map[string]client.Model{
   	"Logical_Switch": &MyLogicalSwitch{},
   })


The ClientDBModel represents the entire Database (or the part of it we're interested in).
Using it, the libovsdb.client package is able to properly encode and decode OVSDB messages
and store them in Model instances.
A client instance is created by simply specifying the connection information and the database model:

     ovs, _ := client.Connect(context.Background(), clientDBModel)

Main API

After creating a OvsdbClient using the Connect() function, we can use a number of CRUD-like
to interact with the database:
List(), Get(), Create(), Update(), Mutate(), Delete().

The specific database table that the operation targets is automatically determined based on the type
of the parameter.

In terms of return values, some of these functions like Create(), Update(), Mutate() and Delete(),
interact with the database so they return list of ovsdb.Operation objects that can be grouped together
and passed to client.Transact().

Others, such as List() and Get(), interact with the client's internal cache and are able to
return Model instances (or a list thereof) directly.

Conditions

Some API functions (Create() and Get()), can be run directly. Others, require us to use
a ConditionalAPI. The ConditionalAPI injects RFC7047 Conditions into ovsdb Operations as well as
uses the Conditions to search the internal cache.

The ConditionalAPI is created using the Where(), WhereCache() and WhereAll() functions.

Where() accepts a Model (pointer to a struct with ovs tags) and a number of Condition instances.
Conditions must refer to fields of the provided Model (via pointer to fields). Example:

	ls = &MyLogicalSwitch {}
	ovs.Where(ls, client.Condition {
		Field: &ls.Ports,
		Function: ovsdb.ConditionIncludes,
		Value: []string{"portUUID"},
	    })

If no client.Condition is provided, the client will use any of fields that correspond to indexes to
generate an appropriate condition. Therefore the following two statements are equivalent:

	ls = &MyLogicalSwitch {UUID:"myUUID"}

	ovs.Where(ls)

	ovs.Where(ls, client.Condition {
		Field: &ls.UUID,
		Function: ovsdb.ConditionEqual,
		Value: "myUUID"},
	    })

Where() accepts multiple Condition instances (through variadic arguments).
If provided, the client will generate multiple operations each matching one condition.
For example, the following operation will delete all the Logical Switches named "foo" OR "bar":

	ops, err := ovs.Where(ls,
		client.Condition {
			Field: &ls.Name
			Function: ovsdb.ConditionEqual,
			Value: "foo",
		},client.Condition {
			Field: &ls.Port,
			Function: ovsdb.ConditionIncludes,
			Value: "bar",
	}).Delete()

To create a Condition that matches all of the conditions simultaneously (i.e: AND semantics), use WhereAll().

Where() or WhereAll() evaluate the provided index values or explicit conditions against the cache and generate
conditions based on the UUIDs of matching models. If no matches are found in the cache, the generated conditions
will be based on the index or condition fields themselves.

A more flexible mechanism to search the cache is available: WhereCache()

WhereCache() accepts a function that takes any Model as argument and returns a boolean.
It is used to search the cache so commonly used with List() function. For example:

	lsList := &[]LogicalSwitch{}
	err := ovs.WhereCache(
	    func(ls *LogicalSwitch) bool {
	    	return strings.HasPrefix(ls.Name, "ext_")
	}).List(lsList)

Server side operations can be executed using WhereCache() conditions but it's not recommended. For each matching
cache element, an operation will be created matching on the "_uuid" column. The number of operations can be
quite large depending on the cache size and the provided function. Most likely there is a way to express the
same condition using Where() or WhereAll() which will be more efficient.

Get

Get() operation is a simple operation capable of retrieving one Model based on some of its schema indexes. E.g:

	ls := &LogicalSwitch{UUID:"myUUID"}
	err := ovs.Get(ls)
	fmt.Printf("Name of the switch is: &s", ls.Name)

List

List() searches the cache and populates a slice of Models. It can be used directly or using WhereCache()

	lsList := &[]LogicalSwitch{}
	err := ovs.List(lsList) // List all elements

	err := ovs.WhereCache(
	    func(ls *LogicalSwitch) bool {
	    	return strings.HasPrefix(ls.Name, "ext_")
	}).List(lsList)

Create

Create returns a list of operations to create the models provided. E.g:

	ops, err := ovs.Create(&LogicalSwitch{Name:"foo")}, &LogicalSwitch{Name:"bar"})

Update
Update returns a list of operations to update the matching rows to match the values of the provided model. E.g:

	ls := &LogicalSwitch{ExternalIDs: map[string]string {"foo": "bar"}}
	ops, err := ovs.Where(...).Update(&ls, &ls.ExternalIDs}

Mutate

Mutate returns a list of operations needed to mutate the matching rows as described by the list of Mutation objects. E.g:

	ls := &LogicalSwitch{}
	ops, err := ovs.Where(...).Mutate(&ls, client.Mutation {
		Field:   &ls.Config,
		Mutator: ovsdb.MutateOperationInsert,
		Value:   map[string]string{"foo":"bar"},
	})

Delete

Delete returns a list of operations needed to delete the matching rows. E.g:

	ops, err := ovs.Where(...).Delete()

*/
package client
