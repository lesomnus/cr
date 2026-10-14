package api

// ManifestCreatedEvent identifies an index insertion on the watch broker.
// It shares the entity's service prefix but is an event, not an RPC.
const ManifestCreatedEvent = "/app.ManifestService/Created"

// ManifestCreatedAction is the action carried by a creation notification.
const ManifestCreatedAction = "created"
