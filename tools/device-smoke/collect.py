"""Run every independent step of a check and report all failures, so an early failing step (doctor) cannot hide a
later one (exec): the 11-22 Windows exec-self bug stayed invisible behind a degraded doctor."""


def run_all(*steps):
    """Each step is (name, callable). Runs them all; raises one RuntimeError naming every failed step."""
    failures = []
    for name, step in steps:
        try:
            step()
        except Exception as error:  # a step failing must not stop the next independent step
            failures.append('%s: %s' % (name, error))
    if failures:
        raise RuntimeError('; '.join(failures))
