from pydantic import BaseModel
from semantic_robot_skill_sdk import Action


class Input(BaseModel):
    pass


async def run(ctx):
    # 模拟 Ability 结果未知、Skill 却返回普通失败的真实故障路径。
    await ctx.execute("move", Action(type="navigation.follow_route", schema_version=1,
                                    parameters={}, timeout_seconds=3))
    ctx.fail("ACTION_FAILED", "action result is unknown")


async def on_stop(ctx, request):
    result = await ctx.execute_stop("hold", Action(type="navigation.follow_route", schema_version=1,
        parameters={"reason": request.reason}, timeout_seconds=3))
    return ctx.stop_outcome(safe=result.status == "succeeded" and result.physical_effect == "confirmed",
                            summary="hold confirmed", physical_state="held")
