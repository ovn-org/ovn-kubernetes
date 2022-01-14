package serverdb

// server_model is a database model for the special _Server database that all
// ovsdb instances export. It reports back status of the server process itself.

//go:generate ../../bin/modelgen --extended -p serverdb -o . _server.ovsschema
