"""Contract tests for the importer; real Metabase UI validation is separate."""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("bvk_import", ROOT / "import.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class FakeAPI:
    """Enforces the v0.64.1 dashboard update shape from official API source."""
    objects = {}
    next_id = 1
    queries = 0

    def __init__(self, url, token, key=False):
        pass

    def call(self, method, path, body=None):
        cls = type(self)
        if path == "card" or path.startswith("card/") and not path.endswith("/query"):
            assert body.get("description") != "", "Metabase rejects blank descriptions"
        if method == "POST" and path.endswith("/query"):
            for p in body.get("parameters", []):
                assert isinstance(p.get("id"), str) and p["id"], "Query parameters require id"
            cls.queries += 1
            return {"status": "completed"}
        if method == "POST":
            ident = cls.next_id
            cls.next_id += 1
            obj = dict(body, id=ident)
            cls.objects[path + "/" + str(ident)] = obj
            return obj
        if method == "GET":
            return cls.objects[path]
        if method == "PUT":
            assert path in cls.objects, path
            obj = cls.objects[path]
            obj.update(body)
            if path.startswith("dashboard/"):
                tabs = {}
                for t in body["tabs"]:
                    if t["id"] < 0:
                        old = t["id"]
                        t["id"] = cls.next_id
                        cls.next_id += 1
                        tabs[old] = t["id"]
                ids = set()
                for dc in obj["dashcards"]:
                    assert set(dc) <= {"id", "size_x", "size_y", "row", "col", "card_id", "action_id", "dashboard_tab_id", "parameter_mappings", "visualization_settings", "inline_parameters", "series"}
                    assert dc["id"] not in ids
                    ids.add(dc["id"])
                    assert dc["row"] >= 0 and dc["col"] >= 0 and dc["col"] + dc["size_x"] <= 24
                    assert dc["card_id"] is None or dc["card_id"] > 0
                    if dc["id"] < 0:
                        dc["id"] = cls.next_id
                        cls.next_id += 1
                    dc["dashboard_tab_id"] = tabs.get(dc["dashboard_tab_id"], dc["dashboard_tab_id"])
                    if body["tabs"]:
                        assert dc["dashboard_tab_id"] in {t["id"] for t in obj["tabs"]}
            return obj
        raise AssertionError((method, path))


class ImportTests(unittest.TestCase):
    def test_repeat_import_tabs_filters_links_and_secret_free_state(self):
        FakeAPI.objects = {}
        FakeAPI.next_id = 1
        FakeAPI.queries = 0
        with tempfile.TemporaryDirectory() as td:
            state = Path(td) / "ids.json"
            argv = ["import.py", "--database-id", "1", "--state", str(state), "--validate-queries"]
            with patch.object(module, "API", FakeAPI), patch.object(sys, "argv", argv), patch.dict(os.environ, {"METABASE_SESSION": "SECRET_SESSION_MARKER"}, clear=True), contextlib.redirect_stdout(io.StringIO()):
                module.main()
                first = json.loads(state.read_text())
                ident_count = FakeAPI.next_id
                module.main()
                second = json.loads(state.read_text())
            self.assertEqual(first, second)
            self.assertEqual(ident_count, FakeAPI.next_id)
            self.assertNotIn("SECRET_SESSION_MARKER", state.read_text())
            self.assertEqual(len(json.loads((ROOT / "manifest.json").read_text())["cards"]), len(first["cards"]))
            self.assertEqual(2*len(first["cards"]), FakeAPI.queries)
            support = FakeAPI.objects["dashboard/" + str(first["dashboards"]["support"])]
            self.assertEqual(["Обзор", "Требуют внимания", "Сотрудники", "Подробно", "Как читать"], [t["name"] for t in support["tabs"]])
            attention = next(t["id"] for t in support["tabs"] if t["name"] == "Требуют внимания")
            self.assertGreaterEqual(sum(dc["dashboard_tab_id"] == attention for dc in support["dashcards"]), 6)
            parameters = {p["id"] for p in support["parameters"]}
            self.assertEqual({"start_date", "end_date", "department", "employee", "company"}, parameters)
            for p in support["parameters"]:
                if p["id"] in {"department", "employee", "company"}:
                    self.assertEqual("card", p["values_source_type"])
                    self.assertIn("label_field", p["values_source_config"])
            overview = next(t['id'] for t in support['tabs'] if t['name'] == 'Обзор')
            overview_cards = [dc['card_id'] for dc in support['dashcards'] if dc['dashboard_tab_id'] == overview]
            self.assertEqual(4, sum(ident in overview_cards for key,ident in first['cards'].items() if key.endswith('_count')))
            # Every tab retains freshness and no two tiles overlap after repeat imports.
            for tab in support['tabs']:
                tiles=[dc for dc in support['dashcards'] if dc['dashboard_tab_id']==tab['id']]
                self.assertIn(first['cards']['status'], [dc['card_id'] for dc in tiles])
                for i,a in enumerate(tiles):
                    for b in tiles[i+1:]:
                        self.assertFalse(a['col'] < b['col']+b['size_x'] and b['col'] < a['col']+a['size_x'] and a['row'] < b['row']+b['size_y'] and b['row'] < a['row']+a['size_y'])
            for dc in support["dashcards"]:
                click = dc["visualization_settings"].get("click_behavior")
                if click:
                    self.assertEqual(parameters, set(click["parameterMapping"]))

    def test_tags_are_stable_and_ticket_number_is_required(self):
        tags = module.tags("SELECT {{ticket_id}}, [[ {{start_date}} ]] {{ticket_id}}")
        self.assertEqual(tags, module.tags("SELECT {{ticket_id}}, [[ {{start_date}} ]] {{ticket_id}}"))
        self.assertTrue(tags["ticket_id"]["required"])
        self.assertFalse(tags["start_date"]["required"])


if __name__ == "__main__":
    unittest.main()
