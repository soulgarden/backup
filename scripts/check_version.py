from pathlib import Path
import yaml
root = Path(__file__).resolve().parents[1]
version = (root / 'VERSION').read_text().strip()
chart = yaml.safe_load((root / 'helm/backup/Chart.yaml').read_text())
assert chart['appVersion'] == version, 'VERSION and appVersion differ'
values=yaml.safe_load((root / 'helm/backup/values.yaml').read_text())
assert values['image']['tag'] == version, 'VERSION and image.tag differ'
