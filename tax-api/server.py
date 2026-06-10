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
        "EIC": [min(req.qualifying_children, 3)],  # qualifying children for EITC
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
    # iitax already has EITC and CTC applied (may be negative for refundable credits)
    refund_or_owed = req.federal_tax_withheld - iitax

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


# ── Minnesota Renters Property Tax Refund (M1PR) ──────────────────────────────

MN_DISCLAIMER = (
    "⚠️ ESTIMATE ONLY — This is not a filed M1PR return. "
    "Amounts are approximate based on published MN Dept of Revenue tables. "
    "Visit a free VITA site for an accurate M1PR prepared by an IRS-certified volunteer: "
    "irs.gov/vita — or visit revenue.state.mn.us for the official M1PR instructions."
)

# 2024 M1PR Renter's Refund copayment table (income limit, copayment rate)
# Source: MN Dept of Revenue M1PR instructions (verify at revenue.state.mn.us)
_M1PR_TABLE = [
    (7_110,  0.00),
    (9_559,  0.01),
    (12_009, 0.02),
    (14_459, 0.03),
    (16_909, 0.04),
    (19_359, 0.05),
    (21_809, 0.06),
    (24_259, 0.07),
    (29_159, 0.08),
    (36_809, 0.09),
    (44_459, 0.10),
    (52_109, 0.11),
    (59_759, 0.12),
    (67_409, 0.13),
    (72_230, 0.14),
]
M1PR_INCOME_LIMIT = 72_230
M1PR_MAX_REFUND   = 2_770   # 2024 maximum renter's refund
M1PR_RENT_TAX_PCT = 0.17    # MN treats 17% of rent as property tax paid
# Seniors (65+) and totally disabled receive an additional 20% on top of base refund
M1PR_SENIOR_BONUS = 0.20


class MNRentersRebateRequest(BaseModel):
    household_income: float = Field(
        description="Total household income from ALL sources — wages, Social Security, unemployment, child support received, etc."
    )
    annual_rent_paid: float = Field(
        description="Total rent paid in Minnesota during the year (from your Certificate of Rent Paid / CRP form)"
    )
    age: int = Field(default=40, description="Age of primary filer")
    is_disabled: bool = Field(default=False, description="Whether filer receives disability benefits (qualifies for senior rate)")
    months_in_mn: int = Field(default=12, description="Number of months rented in Minnesota (1–12)")


class MNRentersRebateResponse(BaseModel):
    eligible: bool
    estimated_refund: float
    net_rent: float
    copayment: float
    copayment_rate_pct: float
    senior_or_disabled_bonus_applied: bool
    income_limit: int
    max_refund: int
    summary: str
    next_steps: str
    disclaimer: str


@app.post("/mn_renters_rebate", response_model=MNRentersRebateResponse)
def mn_renters_rebate(req: MNRentersRebateRequest):
    months = max(1, min(12, req.months_in_mn))

    # Prorate household income and rent if partial-year resident
    income = req.household_income * (months / 12)
    rent   = req.annual_rent_paid  # CRP already covers only MN months

    if income > M1PR_INCOME_LIMIT:
        return MNRentersRebateResponse(
            eligible=False,
            estimated_refund=0.0,
            net_rent=round(rent * M1PR_RENT_TAX_PCT, 2),
            copayment=0.0,
            copayment_rate_pct=0.0,
            senior_or_disabled_bonus_applied=False,
            income_limit=M1PR_INCOME_LIMIT,
            max_refund=M1PR_MAX_REFUND,
            summary=(
                f"Based on a household income of ${req.household_income:,.0f}, "
                f"you are over the {TAX_YEAR} income limit of ${M1PR_INCOME_LIMIT:,} "
                f"for the Minnesota Renters Property Tax Refund."
            ),
            next_steps="You do not qualify for the renter's refund this year. If your income is lower next year, check again.",
            disclaimer=MN_DISCLAIMER,
        )

    # Find copayment rate from table
    copayment_rate = _M1PR_TABLE[-1][1]
    for limit, rate in _M1PR_TABLE:
        if income <= limit:
            copayment_rate = rate
            break

    net_rent   = rent * M1PR_RENT_TAX_PCT
    copayment  = income * copayment_rate
    base_refund = max(0.0, net_rent - copayment)

    # Senior / disability bonus
    is_senior = req.age >= 65 or req.is_disabled
    if is_senior:
        base_refund *= (1 + M1PR_SENIOR_BONUS)

    refund = round(min(base_refund, M1PR_MAX_REFUND), 2)

    if refund == 0:
        summary = (
            f"Based on your household income of ${req.household_income:,.0f} and "
            f"rent paid of ${req.annual_rent_paid:,.0f}, your estimated refund is $0. "
            f"Your copayment (${copayment:,.0f}) exceeds the property-tax portion of your rent (${net_rent:,.0f})."
        )
    else:
        senior_note = " A senior/disability bonus of 20% has been applied." if is_senior else ""
        summary = (
            f"Good news! Based on your household income of ${req.household_income:,.0f} and "
            f"rent paid of ${req.annual_rent_paid:,.0f}, you may qualify for a "
            f"Minnesota Renters Property Tax Refund of approximately ${refund:,.0f}.{senior_note} "
            f"This is filed on Form M1PR, separate from your regular state return."
        )

    next_steps = (
        "To claim this refund: (1) Get your Certificate of Rent Paid (CRP) from your landlord — "
        "they are required by law to give it to you by January 31. "
        "(2) File Form M1PR with the MN Dept of Revenue. "
        "(3) A free VITA site can prepare this for you at no cost. "
        "The deadline to file M1PR is August 15."
    )

    return MNRentersRebateResponse(
        eligible=refund > 0,
        estimated_refund=refund,
        net_rent=round(net_rent, 2),
        copayment=round(copayment, 2),
        copayment_rate_pct=round(copayment_rate * 100, 1),
        senior_or_disabled_bonus_applied=is_senior,
        income_limit=M1PR_INCOME_LIMIT,
        max_refund=M1PR_MAX_REFUND,
        summary=summary,
        next_steps=next_steps,
        disclaimer=MN_DISCLAIMER,
    )


if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=8001)
