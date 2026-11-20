# Contributing

Keep changes focused and describe the behavior they fix. Add a regression test
for a queue/state bug and run the checks in the README with FFmpeg installed.
Avoid committing audio, databases, credentials, or generated binaries.

The public API should keep stable status names and error codes. If a change
needs a new schema, add an explicit migration from version 1; do not silently
recreate an existing database. Document changes to the PCM representation because
they affect digest comparisons.
