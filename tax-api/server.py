"""
VITA Tax Estimator API
Wraps PSL Tax-Calculator for federal income tax and EITC estimates.
All responses include a disclaimer — this is estimation only.
"""

import taxcalc
import pandas as pd
import numpy as np
from fastapi import FastAPI
from pydantic import BaseModel, Field
from typing import Optional
import uvicorn

app = FastAPI(title="VITA Tax Estimator", version="1.0.0")

DISCLAIMER = (
    "⚠️ ESTIMATE ONLY — This is not a filed tax return. "
    "Actual results depend on your complete tax situation. "
    "Visit a free VITA site for an accurate return prepared by an IRS-certified volunteer: "
    "irs.gov/vita"
)

TAX_YEAR = 2024

# Filing status codes for Tax-Calculator
# 1=Single, 2=Married Filing Jointly, 3=Married Filing Separately,
# 4=Head of Household, 5=Qualifying Widow(er)
FILING_STATUS_MAP = {
    "single": 1,
    "married_filing_jointly": 2,
    "married_filing_separately": 3,
    "head_of_household": 4,
    "qualifying_widow": 5,
}


class TaxEstimateRequest(BaseModel):
    filing_status: str = Field(
        description="single | married_filing_jointly | married_filing_separately | head_of_household | qualifying_widow"
    )
    wages: float = Field(default=0.0, description="W-2 wages and salaries")
    self_employment_income: float = Field(default=0.0, description="Net self-employment / gig income")
    other_income: float = Field(default=0.0, description="Interest, dividends, unemployment, other taxable income")
    federal_tax_withheld: float = Field(default=0.0, description="Federal income tax already withheld from paychecks")
    age: int = Field(default=40, description="Age of primary filer")
    spouse_age: int = Field(default=0, description="Age of spouse (0 if not applicable)")
    qualifying_children: int = Field(default=0, description="Number of qualifying children under 17")
    dependents_other: int = Field(default=0, description="Number of other dependents (not children under 17)")


class TaxEstimateResponse(BaseModel):
    federal_income_tax: float
    payroll_tax: float
    total_tax: float
    eitc: float
    child_tax_credit: float
    estimated_refund_or_owed: float
    effective_tax_rate_pct: float
    eitc_eligible: bool
    summary: str
    disclaimer: str


def run_taxcalc(req: TaxEstimateRequest) -> dict:
    mars = FILING_STATUS_MAP.get(req.filing_status.lower(), 1)
    spouse_age = req.spouse_age if mars == 2 else 0

    data = pd.DataFrame({
        "RECID": [1],
        "MARS": [mars],
        "age_head": [req.age],
        "age_spouse": [spouse_age],
        "n24": [req.qualifying_children],       # children under 17 (CTC)
        "nu18": [req.qualifying_children],      # children under 18 (EITC)
        "nu13": [req.qualifying_children],      # children under 13 (CDCC)
        "n1820": [0],
        "n21": [0],
        "XTOT": [1 + (1 if mars == 2 else 0) + req.qualifying_children + req.dependents_other],
        "e00200": [req.wages],                  # wages
        "e00200p": [req.wages],
        "e00200s": [0.0],
        "e00900": [req.self_employment_income], # self-employment income
        "e00900p": [req.self_employment_income],
        "e00900s": [0.0],
        "e00300": [req.other_income],           # interest / other income
        "e02400": [0.0],                        # Social Security benefits
        "e01500": [0.0],                        # pension income
        "e01700": [0.0],                        # taxable pension
        "p22250": [0.0],                        # short-term capital gains
        "p23250": [0.0],                        # long-term capital gains
        "e18400": [0.0],
        "e18500": [0.0],
        "e19200": [0.0],
        "e19800": [0.0],
        "e20100": [0.0],
        "e20400": [0.0],
        "e17500": [0.0],
        "e07300": [0.0],
        "e07400": [0.0],
        "e07600": [0.0],
        "e11200": [0.0],
        "e03150": [0.0],
        "e03210": [0.0],
        "e03220": [0.0],
        "e03230": [0.0],
        "e03270": [0.0],
        "e03240": [0.0],
        "e03290": [0.0],
        "e03300": [0.0],
        "e03400": [0.0],
        "e03500": [0.0],
        "e00700": [0.0],
        "e00800": [0.0],
        "e00600": [0.0],
        "e00650": [0.0],
        "e00900s": [0.0],
        "e01200": [0.0],
        "e24515": [0.0],
        "e24518": [0.0],
        "cmbtp": [0.0],
        "f2441": [min(req.qualifying_children, 3)],
        "ffpos": [1],
        "fips": [27],  # Minnesota
        "s006": [1.0],
        "data_source": [1],
    })

    recs = taxcalc.Records(
        data=data,
        start_year=TAX_YEAR,
        gfactors=None,
        weights=None,
        adjust_ratios=None,
    )
    pol = taxcalc.Policy()
    calc = taxcalc.Calculator(policy=pol, records=recs, verbose=False)
    calc.advance_to_year(TAX_YEAR)
    calc.calc_all()

    iitax = float(calc.array("iitax")[0])
    payrolltax = float(calc.array("payrolltax")[0])
    eitc = float(calc.array("eitc")[0])
    ctc = float(calc.array("c07220")[0])  # child tax credit

    total_income = req.wages + req.self_employment_income + req.other_income
    effective_rate = (iitax / total_income * 100) if total_income > 0 else 0.0
    refund_or_owed = req.federal_tax_withheld - iitax + eitc + ctc

    return {
        "federal_income_tax": round(iitax, 2),
        "payroll_tax": round(payrolltax, 2),
        "total_tax": round(iitax + payrolltax, 2),
        "eitc": round(eitc, 2),
        "child_tax_credit": round(ctc, 2),
        "estimated_refund_or_owed": round(refund_or_owed, 2),
        "effective_tax_rate_pct": round(effective_rate, 1),
        "eitc_eligible": eitc > 0,
    }


@app.post("/estimate", response_model=TaxEstimateResponse)
def estimate_tax(req: TaxEstimateRequest):
    result = run_taxcalc(req)

    refund = result["estimated_refund_or_owed"]
    eitc = result["eitc"]

    if refund >= 0:
        refund_str = f"estimated refund of ${refund:,.0f}"
    else:
        refund_str = f"estimated amount owed of ${abs(refund):,.0f}"

    eitc_str = f" You appear to qualify for the Earned Income Tax Credit (EITC) of approximately ${eitc:,.0f}." if eitc > 0 else ""

    summary = (
        f"Based on the information provided, your estimated federal income tax is ${result['federal_income_tax']:,.0f} "
        f"with an effective tax rate of {result['effective_tax_rate_pct']}%. "
        f"After withholding and credits, you have an {refund_str}.{eitc_str}"
    )

    return TaxEstimateResponse(
        **result,
        summary=summary,
        disclaimer=DISCLAIMER,
    )


@app.get("/health")
def health():
    return {"status": "ok", "tax_year": TAX_YEAR}


if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=8001)
