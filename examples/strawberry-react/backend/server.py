from fastapi import FastAPI
from strawberry.fastapi import GraphQLRouter

from schema import schema

graphql_app = GraphQLRouter[None, None](schema, graphql_ide=None)

app = FastAPI()
app.include_router(graphql_app, prefix="/graphql")
