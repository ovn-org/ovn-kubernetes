/*
Package updates provides an utility to perform and aggregate model updates.

As input, it supports OVSDB Operations, RowUpdate or RowUpdate2 notations via
the corresponding Add methods.

As output, it supports both OVSDB RowUpdate2 as well as model notation via the
corresponding ForEach iterative methods.

Several updates can be added and will be merged with any previous updates even
if they are for the same model. If several updates for the same model are
aggregated, the user is responsible that the provided model to be updated
matches the updated model of the previous update.
*/
package updates
