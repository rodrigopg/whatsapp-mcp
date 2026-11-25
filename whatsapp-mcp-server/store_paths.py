import os
from pathlib import Path

BRIDGE_DIR = Path(__file__).resolve().parent.parent / "whatsapp-bridge"
configured = os.environ.get("WHATSAPP_STORE_DIR")
if configured and not Path(configured).is_absolute():
    raise ValueError("WHATSAPP_STORE_DIR must be an absolute path")
STORE_DIR = Path(os.path.abspath(configured or BRIDGE_DIR / "store"))
