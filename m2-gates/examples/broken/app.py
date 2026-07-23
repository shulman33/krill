# The B3 gate app: builds fine, crashes at import time. The deploy response
# and the logs tool must hand back this traceback, structured, so an agent
# can fix the bug without ever leaving its tool loop.
import sqlite3
import threading

import fastapi
from pydantic import BaseModel

app = FastAPI()  # NameError: imported the module, forgot the class import


@app.get("/")
def index():
    return {"app": "broken"}
