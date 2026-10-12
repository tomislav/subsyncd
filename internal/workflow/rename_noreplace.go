package workflow

import "errors"

// errRenameNoReplaceUnsupported reports a filesystem (or platform) that
// cannot rename without replacing the destination atomically. A move that
// would need it gives up instead of checking and then renaming, which could
// overwrite a file created in between.
var errRenameNoReplaceUnsupported = errors.New("rename without replacing is not supported")
