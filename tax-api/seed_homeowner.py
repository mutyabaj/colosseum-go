"""
Run inside the Colosseum container to register the mn_homeowner_rebate tool.
"""
import sqlite3, json, uuid, datetime

db_path = "/data/colosseum.db"
now = datetime.datetime.utcnow().isoformat() + "Z"

tool = {
    "id": str(uuid.uuid4()),
    "name": "mn_homeowner_rebate",
    "description": (
        "Estimate the Minnesota Homeowner Property Tax Refund (Form M1PR). "
        "Use this when a homeowner (not renter) in Minnesota asks about property tax relief. "
        "Also checks if a Special Property Tax Refund may apply for large year-over-year increases. "
        "Always relay the disclaimer field verbatim in your response."
    ),
    "input_schema": json.dumps({
        "type": "object",
        "properties": {
            "household_income": {
                "type": "number",
                "description": "Total household income from ALL sources — wages, Social Security, pensions, unemployment, etc.",
            },
            "property_taxes_paid": {
                "type": "number",
                "description": "Net property taxes paid on the homestead this year (from property tax statement)",
            },
            "age": {
                "type": "integer",
                "description": "Age of primary filer (default 40)",
            },
            "is_disabled": {
                "type": "boolean",
                "description": "Whether filer receives disability benefits (default false)",
            },
            "prior_year_property_taxes": {
                "type": "number",
                "description": "Property taxes paid last year — needed to check for Special Refund eligibility (default 0 to skip)",
            },
        },
        "required": ["household_income", "property_taxes_paid"],
    }),
    "kind": "http_tool",
    "config": json.dumps({
        "url": "http://tax-api:8001/mn_homeowner_rebate",
        "method": "POST",
        "timeout_seconds": 30,
    }),
}

db = sqlite3.connect(db_path)
cur = db.cursor()

cur.execute("""
    INSERT INTO tool_defs (id, name, description, input_schema_json, kind, config_json, enabled, is_builtin, created_at, updated_at)
    VALUES (:id, :name, :description, :input_schema, :kind, :config, 1, 0, :now, :now)
    ON CONFLICT(name) DO UPDATE SET
        description=excluded.description,
        input_schema_json=excluded.input_schema_json,
        config_json=excluded.config_json,
        updated_at=excluded.updated_at
""", {**tool, "now": now})
db.commit()
print(f"Tool registered: mn_homeowner_rebate (id={tool['id']})")
print("Add it to your VITA agent's tool list in the Colosseum UI.")
