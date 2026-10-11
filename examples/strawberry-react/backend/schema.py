import strawberry

COUNTER_ID = "theOneAndOnly"
_counter: int = 0


@strawberry.type
class Counter:
    id: str
    value: int


def getCounter() -> Counter:
    return Counter(id=COUNTER_ID, value=_counter)


@strawberry.type
class Query:
    counter: Counter = strawberry.field(resolver=getCounter)


@strawberry.type
class Mutation:
    @strawberry.mutation
    def increment(self) -> Counter:
        global _counter
        _counter += 1
        return Counter(id=COUNTER_ID, value=_counter)


schema = strawberry.Schema(query=Query, mutation=Mutation)
