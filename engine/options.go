package engine

// Option configures a gateway New builds. Options apply after every
// workstream's state is built and before anything serves, in the order given;
// an option sets state and must not start work. A workstream's options are
// declared in its own files (WithPopulator is the PA workstream's).
type Option func(*Gateway)
