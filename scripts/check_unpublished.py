"""Fail closed before publishing; an existing tag must never be overwritten."""
import re,sys,urllib.error,urllib.request
from pathlib import Path
version=Path(__file__).resolve().parents[1].joinpath('VERSION').read_text().strip()
assert re.fullmatch(r'\d+\.\d+\.\d+',version)
try:
    with urllib.request.urlopen('https://hub.docker.com/v2/repositories/soulgarden/backup/tags/'+version,timeout=15):
        raise SystemExit('Refusing to overwrite existing image tag '+version)
except urllib.error.HTTPError as error:
    if error.code!=404:raise SystemExit('Cannot confirm tag absence; publication blocked') from None
print('Image tag is unpublished: '+version)
