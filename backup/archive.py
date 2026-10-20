import hashlib
import json
import re

INVENTORY_SQL = r"""
SELECT json_build_object(
 'server_version', current_setting('server_version'),
 'server_version_num', current_setting('server_version_num')::int,
 'schemas', (SELECT coalesce(json_agg(json_build_object('oid',oid::bigint,'name',nspname)
 ORDER BY oid),'[]'::json) FROM pg_namespace
 WHERE nspname !~ '^pg_' AND nspname <> 'information_schema'),
 'tables', (SELECT coalesce(json_agg(json_build_object(
 'oid',c.oid::bigint,'schema',n.nspname,'table',c.relname,'kind',c.relkind,
 'rls',c.relrowsecurity,'select',has_table_privilege(c.oid,'SELECT'),
 'parents',(SELECT coalesce(json_agg(inhparent::bigint ORDER BY inhparent),'[]'::json)
 FROM pg_inherits WHERE inhrelid=c.oid)) ORDER BY c.oid),'[]'::json)
 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE c.relkind IN ('r','p','f') AND n.nspname !~ '^pg_' AND n.nspname<>'information_schema'));
"""


def inventory(processes, env):
    return json.loads(processes.run(["psql", "-X", "--no-password", "-At",
                                     "-v", "ON_ERROR_STOP=1", "-c", INVENTORY_SQL], env))


def dump_args(config, path):
    b = config["backup"]
    args = ["pg_dump", "--no-password", "--format=custom",
            f"--compress=gzip:{b['compressionLevel']}",
            f"--lock-wait-timeout={config['postgresql']['lockWaitTimeoutSeconds']}s",
            f"--file={path}"]
    for setting, flag in (("includeSchemas", "schema"), ("excludeSchemas", "exclude-schema"),
                          ("excludeTables", "exclude-table"),
                          ("excludeTableData", "exclude-table-data")):
        args.extend(f"--{flag}={value}" for value in b[setting])
    args.append("--large-objects" if b["includeLargeObjects"] else "--no-large-objects")
    if not b["includeAcl"]:
        args.append("--no-acl")
    if b["strictNames"]:
        args.append("--strict-names")
    return args


def versions(processes, config, inv):
    major = config["postgresql"]["majorVersion"]
    result = {"server_version": inv["server_version"]}
    if inv["server_version_num"] // 10000 != major:
        raise ValueError("SERVER_MAJOR_MISMATCH")
    for executable in ("pg_dump", "pg_restore"):
        version = processes.run([executable, "--version"]).strip()
        match = re.search(r"\(PostgreSQL\) (\d+)\.", version)
        if not match or int(match[1]) != major:
            raise ValueError("CLIENT_MAJOR_MISMATCH")
        result[executable + "_version"] = version
    return result


def toc_entries(text):
    entries = []
    for line in text.splitlines():
        m = re.match(r"^(\d+);\s+(\d+)\s+(\d+)\s+(.*)$", line)
        if not m:
            continue
        tableoid, oid, suffix = int(m[2]), int(m[3]), m[4]
        # Catalog IDs disambiguate TABLE in schema "DATA" from TABLE DATA.
        # pg_class=1259 and pg_namespace=2615 are fixed PG16 catalog OIDs.
        kind = None
        if tableoid == 1259 and suffix.startswith(("TABLE ", "FOREIGN TABLE ")):
            kind = "TABLE"
        elif tableoid == 0 and suffix.startswith("TABLE DATA "):
            kind = "TABLE DATA"
        elif tableoid == 2615 and suffix.startswith("SCHEMA "):
            kind = "SCHEMA"
        if kind:
            entries.append((tableoid, oid, kind))
    return entries


def validate_toc(text, inv, after, policy):
    if inv != after:
        raise ValueError("CATALOG_CHANGED")
    entries = toc_entries(text)
    definitions = {oid for _, oid, kind in entries if kind == "TABLE"}
    data = {oid for _, oid, kind in entries if kind == "TABLE DATA"}
    schemas = {oid for _, oid, kind in entries if kind == "SCHEMA"}
    tables = inv["tables"]
    if len(data) < policy["minTableDataEntries"]:
        raise ValueError("TOO_FEW_TABLE_DATA")
    for name in policy["requiredSchemas"]:
        found = next((s for s in inv["schemas"] if s["name"] == name), None)
        if found is None or (found["oid"] not in schemas and not any(
                t["schema"] == name and t["oid"] in definitions for t in tables)):
            raise ValueError("MISSING_SCHEMA")
    for required in policy["requiredTables"]:
        table = next((t for t in tables if t["schema"] == required["schema"]
                      and t["table"] == required["table"]), None)
        if table is None or table["oid"] not in definitions:
            raise ValueError("MISSING_TABLE")
        if required["requireData"]:
            selected = {table["oid"]}
            while True:
                children = {t["oid"] for t in tables if selected.intersection(t["parents"])}
                expanded = selected | children
                if expanded == selected:
                    break
                selected = expanded
            if any(t["kind"] == "f" and t["oid"] in selected for t in tables):
                raise ValueError("FOREIGN_TABLE_DATA_UNSUPPORTED")
            leaves = [t for t in tables if t["oid"] in selected and t["kind"] == "r"]
            if any(t["rls"] or not t["select"] or t["oid"] not in data for t in leaves):
                raise ValueError("MISSING_TABLE_DATA")
    return {"table_data_entries": len(data), "required_objects_checked": True}


def hash_file(path, chunk=8 * 1024 * 1024):
    digest = hashlib.sha256()
    size = 0
    with open(path, "rb") as source:
        while block := source.read(chunk):
            size += len(block)
            digest.update(block)
    return size, digest.hexdigest()
