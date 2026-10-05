from pydantic import BaseModel
from semantic_robot_skill_sdk import Action


class Input(BaseModel):
    wait_for_action: bool = False


class Reply(BaseModel):
    action: str


async def run(ctx):
    if ctx.input(Input).wait_for_action:
        await ctx.execute("move", Action(type="navigation.follow_route", schema_version=1, parameters={}, timeout_seconds=5))
    await ctx.request_agent("decision", "action needs a decision", {"stage": "move"}, Reply)


async def on_stop(ctx, request):
    result = await ctx.execute_stop("hold", Action(type="navigation.follow_route", schema_version=1,
        parameters={"reason": request.reason}, timeout_seconds=3))
    return ctx.stop_outcome(safe=result.status == "succeeded" and result.physical_effect == "confirmed",
        summary="hold confirmed", physical_state="held")
