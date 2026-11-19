from langchain_deepseek import deepseek
from langchain_core.prompts import ChatPromptTemplate

#初始化模型
llm = deepseek(
    model="deepseek-chat"
)


#Prompt模版
prompt =ChatPromptTemplate.from_template(
    "请解释：{topic}"
)

#创建Chain
chain = prompt | llm

#调用
result = chain.invoke({
    "topic":"什么是Transformer"
})

print(result.content)