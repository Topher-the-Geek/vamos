import pytest
from strawberry.types.execution import ExecutionResult

from schema import schema


@pytest.mark.asyncio
async def test_mutation():
    resp = await schema.execute(
        """
            mutation {
                increment {
                    value
                }
            }
        """
    )

    assert isinstance(resp, ExecutionResult)
    assert resp.errors is None
    assert resp.data is not None
    assert resp.data["increment"] == {"value": 1}
