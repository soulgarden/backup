"""Fixed events: arbitrary diagnostics can be suppressed by logalert's NOT filter."""
import json
import re


def emit(event, stage, run_id, code="OK", level="INFO"):
    assert re.fullmatch(r"[a-z_]+", event)
    assert re.fullmatch(r"[a-z_]+", stage)
    assert re.fullmatch(r"[A-Z_]+", code)
    assert re.fullmatch(r"[0-9TZ-]+-[a-f0-9]+", run_id)
    print(json.dumps(dict(level=level, event=event, stage=stage, code=code,
                          run_id=run_id), separators=(",", ":")), flush=True)
