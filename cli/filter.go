package cli

// Keep the Watch filter language in the process; the browser sandbox imports
// cmd and serves only unfiltered watches without carrying the CEL runtime.
import _ "github.com/lesomnus/payday/resource/resourcecel"
