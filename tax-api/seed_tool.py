"""
Run this once after deploying the tax-api container to register
the estimate_federal_tax tool in Colosseum's database.
"""
import sqlite3, json, uuid, datetime

db_path = "/data/colosseum.db"

tool = {
    "id": str(uuid.uuid4()),
    "name": "estimate_federal_tax",
    "description": (
        "Estimate a client's federal income tax, EITC eligibility, Child Tax Credit, "
        "and projected refund or amount owed. "
        "Use this whenever a client asks about their taxes, refund, or EITC. "
        "Always relay the disclaimer field verbatim in your response."
    ),
    "input_schema": json.dumps({
        "type": "object",
        "properties": {
            "filing_status": {
                "type": "string",
                "enum": ["single", "married_filing_jointly", "married_filing_separately", "head_of_household", "qualifying_widow"],
                "description": "Tax filing status"
            },
            "wages": {"type": "number", "description": "W-2 wages and salaries (default 0)"},
            "self_employment_income": {"type": "number", "description": "Net self-employment or gig income (default 0)"},
            "other_income": {"type": "number", "description": "Interest, unemployment, or other taxable income (default 0)"},
            "federal_tax_withheld": {"type": "number", "description": "Federal income tax already withheld from paychecks (default 0)"},
            "age": {"type": "integer", "description": "Age of primary filer"},
            "spouse_age": {"type": "integer", "description": "Age of spouse, 0 if not applicable"},
            "qualifying_children": {"type": "integer", "description": "Number of qualifying children under 17"},
            "dependents_other": {"type": "integer", "description": "Number of other dependents (not children under 17)"},
        },
        "required": ["filing_status"],
    }),
    "kind": "http_tool",
    "config": json.dumps({
        "url": "http://tax-api:8001/estimate",
        "method": "POST",
        "timeout_seconds": 30,
    }),
    "now": datetime.datetime.utcnow().isoformat() + "Z",
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
""", tool)
db.commit()
print(f"Tool registered: estimate_federal_tax (id={tool['id']})")
print("Add it to your VITA agent's tool list in the Colosseum UI.")
